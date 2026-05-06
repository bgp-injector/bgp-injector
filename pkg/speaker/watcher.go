package speaker

import (
	"context"
	"net"
	"strings"
	"sync"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/bgp-injector/bgp-injector/pkg/config"
)

// Announcer is implemented by Speaker and can be replaced in tests.
type Announcer interface {
	AnnounceIPv4(ctx context.Context, cidr, nexthop string) error
	WithdrawIPv4(ctx context.Context, cidr, nexthop string) error
	AnnounceIPv6(ctx context.Context, cidr, nexthop string) error
	WithdrawIPv6(ctx context.Context, cidr, nexthop string) error
}

type announcedRoutes struct {
	ipv4Prefixes []string
	ipv6Prefixes []string
	nexthop4     string
	nexthop6     string
}

// Watcher watches pods on a specific node and manages BGP route announcements.
type Watcher struct {
	announcer      Announcer
	defaults       config.Defaults
	nodeName       string
	k8s            kubernetes.Interface
	log            *zap.Logger
	gracefulRestart bool

	mu        sync.Mutex
	announced map[types.UID]*announcedRoutes
	pending   map[types.UID]struct{} // pods with prefixes waiting to become ready
}

func NewWatcher(announcer Announcer, defaults config.Defaults, nodeName string, k8s kubernetes.Interface, log *zap.Logger, gracefulRestart bool) *Watcher {
	return &Watcher{
		announcer:      announcer,
		defaults:       defaults,
		nodeName:       nodeName,
		k8s:            k8s,
		log:            log,
		gracefulRestart: gracefulRestart,
		announced:      make(map[types.UID]*announcedRoutes),
		pending:        make(map[types.UID]struct{}),
	}
}

// Run starts the pod informer and blocks until ctx is cancelled, then withdraws all announced routes.
func (w *Watcher) Run(ctx context.Context) {
	w.log.Info("watcher started", zap.String("node", w.nodeName))

	factory := informers.NewSharedInformerFactoryWithOptions(
		w.k8s,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", w.nodeName).String()
		}),
	)

	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.onPod(ctx, obj) },
		UpdateFunc: func(_, obj interface{}) { w.onPod(ctx, obj) },
		DeleteFunc: func(obj interface{}) { w.onDelete(ctx, obj) },
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	<-ctx.Done()

	if w.gracefulRestart {
		w.log.Info("shutting down with graceful restart enabled, routes held by peer")
		return
	}

	w.log.Info("shutting down, withdrawing all routes")
	w.mu.Lock()
	defer w.mu.Unlock()
	for uid := range w.announced {
		w.withdraw(context.Background(), uid, w.log)
	}
}

func (w *Watcher) onPod(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	cfg, err := config.ParseFromAnnotations(pod.Annotations, w.defaults)
	if err != nil {
		w.log.Error("invalid BGP annotations",
			zap.String("pod", pod.Name),
			zap.String("namespace", pod.Namespace),
			zap.Error(err))
		return
	}

	log := w.log.With(
		zap.String("pod", pod.Name),
		zap.String("namespace", pod.Namespace),
	)

	nexthop4, nexthop6 := podIPs(pod)
	ready := isPodReady(pod)
	shouldAnnounce := !cfg.GateOnReady || ready

	w.mu.Lock()
	defer w.mu.Unlock()

	current, wasAnnounced := w.announced[pod.UID]
	_, wasPending := w.pending[pod.UID]

	if !cfg.HasPrefixes() {
		if wasAnnounced {
			log.Info("BGP annotations removed, withdrawing routes")
			w.withdraw(ctx, pod.UID, log)
		}
		delete(w.pending, pod.UID)
		return
	}

	log = log.With(zap.Strings("ipv4", cfg.IPv4Prefixes), zap.Strings("ipv6", cfg.IPv6Prefixes))

	switch {
	case shouldAnnounce && wasAnnounced:
		if routesChanged(current, cfg, nexthop4, nexthop6) {
			log.Info("BGP annotations changed, resyncing routes")
			w.withdraw(ctx, pod.UID, log)
			w.announce(ctx, pod, cfg, nexthop4, nexthop6, log)
		}
		delete(w.pending, pod.UID)

	case shouldAnnounce && !wasAnnounced:
		if wasPending {
			log.Info("pod ready, announcing routes")
			delete(w.pending, pod.UID)
		} else {
			log.Info("new BGP-annotated pod, announcing routes")
		}
		w.announce(ctx, pod, cfg, nexthop4, nexthop6, log)

	case !shouldAnnounce && wasAnnounced:
		log.Info("pod not ready, withdrawing routes")
		w.pending[pod.UID] = struct{}{}
		w.withdraw(ctx, pod.UID, log)

	case !shouldAnnounce && !wasPending:
		log.Info("new BGP-annotated pod, waiting for readiness")
		w.pending[pod.UID] = struct{}{}
	}
}

func (w *Watcher) onDelete(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			pod, ok = tombstone.Obj.(*corev1.Pod)
			if !ok {
				return
			}
		} else {
			return
		}
	}

	log := w.log.With(zap.String("pod", pod.Name), zap.String("namespace", pod.Namespace))

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, wasPending := w.pending[pod.UID]; wasPending {
		log.Info("BGP-annotated pod deleted before becoming ready")
		delete(w.pending, pod.UID)
		return
	}
	if _, wasAnnounced := w.announced[pod.UID]; wasAnnounced {
		log.Info("pod deleted, withdrawing routes")
		w.withdraw(ctx, pod.UID, log)
	}
}

// announce must be called with w.mu held.
func (w *Watcher) announce(ctx context.Context, pod *corev1.Pod, cfg *config.PodBGPConfig, nexthop4, nexthop6 string, log *zap.Logger) {
	routes := &announcedRoutes{nexthop4: nexthop4, nexthop6: nexthop6}

	for _, prefix := range cfg.IPv4Prefixes {
		if nexthop4 == "" {
			log.Warn("skipping IPv4 prefix: pod has no IPv4 address", zap.String("prefix", prefix))
			continue
		}
		if err := w.announcer.AnnounceIPv4(ctx, prefix, nexthop4); err != nil {
			log.Error("failed to announce IPv4 prefix", zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		routes.ipv4Prefixes = append(routes.ipv4Prefixes, prefix)
		log.Info("route added", zap.String("prefix", prefix), zap.String("nexthop", nexthop4))
	}

	for _, prefix := range cfg.IPv6Prefixes {
		if nexthop6 == "" {
			log.Warn("skipping IPv6 prefix: pod has no IPv6 address", zap.String("prefix", prefix))
			continue
		}
		if err := w.announcer.AnnounceIPv6(ctx, prefix, nexthop6); err != nil {
			log.Error("failed to announce IPv6 prefix", zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		routes.ipv6Prefixes = append(routes.ipv6Prefixes, prefix)
		log.Info("route added", zap.String("prefix", prefix), zap.String("nexthop", nexthop6))
	}

	if len(routes.ipv4Prefixes) > 0 || len(routes.ipv6Prefixes) > 0 {
		w.announced[pod.UID] = routes
	}
}

// withdraw must be called with w.mu held.
func (w *Watcher) withdraw(ctx context.Context, uid types.UID, log *zap.Logger) {
	routes, ok := w.announced[uid]
	if !ok {
		return
	}
	delete(w.announced, uid)

	for _, prefix := range routes.ipv4Prefixes {
		if err := w.announcer.WithdrawIPv4(ctx, prefix, routes.nexthop4); err != nil {
			log.Warn("failed to withdraw IPv4 prefix", zap.String("prefix", prefix), zap.Error(err))
		} else {
			log.Info("route withdrawn", zap.String("prefix", prefix))
		}
	}
	for _, prefix := range routes.ipv6Prefixes {
		if err := w.announcer.WithdrawIPv6(ctx, prefix, routes.nexthop6); err != nil {
			log.Warn("failed to withdraw IPv6 prefix", zap.String("prefix", prefix), zap.Error(err))
		} else {
			log.Info("route withdrawn", zap.String("prefix", prefix))
		}
	}
}

func routesChanged(current *announcedRoutes, cfg *config.PodBGPConfig, nexthop4, nexthop6 string) bool {
	if current.nexthop4 != nexthop4 || current.nexthop6 != nexthop6 {
		return true
	}
	return !prefixSetEqual(current.ipv4Prefixes, cfg.IPv4Prefixes) ||
		!prefixSetEqual(current.ipv6Prefixes, cfg.IPv6Prefixes)
}

func prefixSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v]--; counts[v] < 0 {
			return false
		}
	}
	return true
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podIPs(pod *corev1.Pod) (ipv4, ipv6 string) {
	for _, podIP := range pod.Status.PodIPs {
		ip := net.ParseIP(strings.TrimSpace(podIP.IP))
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			ipv4 = ip.String()
		} else {
			ipv6 = ip.String()
		}
	}
	return
}
