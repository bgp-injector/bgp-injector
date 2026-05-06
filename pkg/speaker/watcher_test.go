package speaker

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bgp-injector/bgp-injector/pkg/config"
)

// fakeAnnouncer records announce/withdraw calls for assertions.
type fakeAnnouncer struct {
	mu        sync.Mutex
	announced []string // "v4:cidr:nexthop"
	withdrawn []string // "v4:cidr:nexthop"
}

func (f *fakeAnnouncer) AnnounceIPv4(_ context.Context, cidr, nexthop string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announced = append(f.announced, "v4:"+cidr+":"+nexthop)
	return nil
}

func (f *fakeAnnouncer) WithdrawIPv4(_ context.Context, cidr, nexthop string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawn = append(f.withdrawn, "v4:"+cidr+":"+nexthop)
	return nil
}

func (f *fakeAnnouncer) AnnounceIPv6(_ context.Context, cidr, nexthop string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announced = append(f.announced, "v6:"+cidr+":"+nexthop)
	return nil
}

func (f *fakeAnnouncer) WithdrawIPv6(_ context.Context, cidr, nexthop string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawn = append(f.withdrawn, "v6:"+cidr+":"+nexthop)
	return nil
}

func newTestWatcher(ann Announcer) *Watcher {
	return NewWatcher(ann, config.Defaults{GateOnReady: true}, "node-1", fake.NewSimpleClientset(), zap.NewNop())
}

func readyPod(uid types.UID, prefixes string, podIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      string(uid),
			Namespace: "default",
			UID:       uid,
			Annotations: map[string]string{
				config.AnnotationIPv4Prefixes: prefixes,
			},
		},
		Status: corev1.PodStatus{
			PodIPs:     []corev1.PodIP{{IP: podIP}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// isPodReady tests

func TestIsPodReady_Ready(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}}
	if !isPodReady(pod) {
		t.Error("expected ready=true")
	}
}

func TestIsPodReady_NotReady(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}}}
	if isPodReady(pod) {
		t.Error("expected ready=false")
	}
}

func TestIsPodReady_NoConditions(t *testing.T) {
	if isPodReady(&corev1.Pod{}) {
		t.Error("expected ready=false for pod with no conditions")
	}
}

// podIPs tests

func TestPodIPs_IPv4Only(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}}}}
	v4, v6 := podIPs(pod)
	if v4 != "10.0.0.1" {
		t.Errorf("ipv4: got %q, want 10.0.0.1", v4)
	}
	if v6 != "" {
		t.Errorf("ipv6: got %q, want empty", v6)
	}
}

func TestPodIPs_DualStack(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{PodIPs: []corev1.PodIP{
		{IP: "10.0.0.1"}, {IP: "fc00::1"},
	}}}
	v4, v6 := podIPs(pod)
	if v4 != "10.0.0.1" {
		t.Errorf("ipv4: got %q, want 10.0.0.1", v4)
	}
	if v6 != "fc00::1" {
		t.Errorf("ipv6: got %q, want fc00::1", v6)
	}
}

func TestPodIPs_Empty(t *testing.T) {
	v4, v6 := podIPs(&corev1.Pod{})
	if v4 != "" || v6 != "" {
		t.Errorf("expected empty, got %q %q", v4, v6)
	}
}

// Watcher.onPod / onDelete tests

func TestOnPod_ReadyWithPrefixes_Announces(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 1 || ann.announced[0] != "v4:1.0.0.0/24:10.0.0.2" {
		t.Errorf("announced: got %v, want [v4:1.0.0.0/24:10.0.0.2]", ann.announced)
	}
}

func TestOnPod_NotReadyWithGateOnReady_DoesNotAnnounce(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")
	pod.Status.Conditions[0].Status = corev1.ConditionFalse

	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 0 {
		t.Errorf("expected no announcements, got %v", ann.announced)
	}
}

func TestOnPod_GateOnReadyFalse_AnnouncesWhenNotReady(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := NewWatcher(ann, config.Defaults{GateOnReady: false}, "node-1", fake.NewSimpleClientset(), zap.NewNop())
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")
	pod.Status.Conditions[0].Status = corev1.ConditionFalse

	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 1 {
		t.Errorf("expected 1 announcement, got %v", ann.announced)
	}
}

func TestOnPod_NoPrefixes_DoesNotAnnounce(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "uid-1"},
		Status: corev1.PodStatus{
			PodIPs:     []corev1.PodIP{{IP: "10.0.0.2"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 0 {
		t.Errorf("expected no announcements for pod without prefixes, got %v", ann.announced)
	}
}

func TestOnPod_BecomesNotReady_Withdraws(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.withdrawn) != 1 || ann.withdrawn[0] != "v4:1.0.0.0/24:10.0.0.2" {
		t.Errorf("withdrawn: got %v, want [v4:1.0.0.0/24:10.0.0.2]", ann.withdrawn)
	}
}

func TestOnDelete_Withdraws(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)
	w.onDelete(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.withdrawn) != 1 {
		t.Errorf("expected 1 withdrawal after delete, got %v", ann.withdrawn)
	}
}

func TestOnDelete_NotAnnounced_NoOp(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onDelete(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.withdrawn) != 0 {
		t.Errorf("expected no withdrawals for unannounced pod, got %v", ann.withdrawn)
	}
}

func TestOnPod_AnnotationRemoved_Withdraws(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)
	// Remove annotations.
	pod.Annotations = nil
	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.withdrawn) != 1 {
		t.Errorf("expected 1 withdrawal after annotation removal, got %v", ann.withdrawn)
	}
}

func TestOnPod_PrefixesChanged_ResyncsRoutes(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)
	// Change to a different prefix.
	pod.Annotations[config.AnnotationIPv4Prefixes] = `["2.0.0.0/24"]`
	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.withdrawn) != 1 || ann.withdrawn[0] != "v4:1.0.0.0/24:10.0.0.2" {
		t.Errorf("expected old prefix withdrawn, got %v", ann.withdrawn)
	}
	if len(ann.announced) != 2 || ann.announced[1] != "v4:2.0.0.0/24:10.0.0.2" {
		t.Errorf("expected new prefix announced, got %v", ann.announced)
	}
}

func TestOnPod_PrefixesUnchanged_NoResync(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "10.0.0.2")

	w.onPod(context.Background(), pod)
	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 1 {
		t.Errorf("expected no resync for unchanged annotations, got %d announces", len(ann.announced))
	}
}

func TestOnPod_NoIPv4Address_SkipsIPv4Prefix(t *testing.T) {
	ann := &fakeAnnouncer{}
	w := newTestWatcher(ann)
	pod := readyPod("uid-1", `["1.0.0.0/24"]`, "")
	pod.Status.PodIPs = nil

	w.onPod(context.Background(), pod)

	ann.mu.Lock()
	defer ann.mu.Unlock()
	if len(ann.announced) != 0 {
		t.Errorf("expected no announcements when pod has no IP, got %v", ann.announced)
	}
}
