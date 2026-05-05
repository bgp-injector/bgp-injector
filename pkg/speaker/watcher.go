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
	announcer Announcer
	defaults  config.Defaults
	nodeName  string
	k8s       kubernetes.Interface
	log       *zap.Logger

	mu        sync.Mutex
	announced map[types.UID]*announcedRoutes
}

func NewWatcher(announcer Announcer, defaults config.Defaults, nodeName string, k8s kubernetes.Interface, log *zap.Logger) *Watcher {
	return &Watcher{
		announcer: announcer,
		defaults:  defaults,
		nodeName:  nodeName,
		k8s:       k8s,
		log:       log,
		announced: make(map[types.UID]*announcedRoutes),
	}
}

// Run starts the pod informer and blocks until ctx is cancelled, then withdraws all announced routes.
func (w *Watcher) Run(ctx context.Context) {
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

	w.mu.Lock()
	defer w.mu.Unlock()
	for uid := range w.announced {
		w.withdraw(context.Background(), uid)
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
	if !cfg.HasPrefixes() {
		return
	}

	nexthop4, nexthop6 := podIPs(pod)
	ready := isPodReady(pod)
	shouldAnnounce := !cfg.GateOnReady || ready

	w.mu.Lock()
	defer w.mu.Unlock()

	_, wasAnnounced := w.announced[pod.UID]

	switch {
	case shouldAnnounce && !wasAnnounced:
		w.announce(ctx, pod, cfg, nexthop4, nexthop6)
	case !shouldAnnounce && wasAnnounced:
		w.withdraw(ctx, pod.UID)
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
	w.mu.Lock()
	defer w.mu.Unlock()
	w.withdraw(ctx, pod.UID)
}

// announce must be called with w.mu held.
func (w *Watcher) announce(ctx context.Context, pod *corev1.Pod, cfg *config.PodBGPConfig, nexthop4, nexthop6 string) {
	log := w.log.With(zap.String("pod", pod.Name), zap.String("namespace", pod.Namespace))
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
		log.Info("announced IPv4 prefix", zap.String("prefix", prefix), zap.String("nexthop", nexthop4))
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
		log.Info("announced IPv6 prefix", zap.String("prefix", prefix), zap.String("nexthop", nexthop6))
	}

	if len(routes.ipv4Prefixes) > 0 || len(routes.ipv6Prefixes) > 0 {
		w.announced[pod.UID] = routes
	}
}

// withdraw must be called with w.mu held.
func (w *Watcher) withdraw(ctx context.Context, uid types.UID) {
	routes, ok := w.announced[uid]
	if !ok {
		return
	}
	delete(w.announced, uid)

	for _, prefix := range routes.ipv4Prefixes {
		if err := w.announcer.WithdrawIPv4(ctx, prefix, routes.nexthop4); err != nil {
			w.log.Warn("failed to withdraw IPv4 prefix", zap.String("prefix", prefix), zap.Error(err))
		} else {
			w.log.Info("withdrew IPv4 prefix", zap.String("prefix", prefix))
		}
	}
	for _, prefix := range routes.ipv6Prefixes {
		if err := w.announcer.WithdrawIPv6(ctx, prefix, routes.nexthop6); err != nil {
			w.log.Warn("failed to withdraw IPv6 prefix", zap.String("prefix", prefix), zap.Error(err))
		} else {
			w.log.Info("withdrew IPv6 prefix", zap.String("prefix", prefix))
		}
	}
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
