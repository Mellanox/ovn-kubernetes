package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	coordinationv1 "k8s.io/api/coordination/v1"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// defaultLeaseDurationSeconds is the default duration of the lease in seconds.
	// This the default value for corev1.Node leases
	defaultLeaseDurationSeconds = 40
	// defaultLeaseNS is the default namespace for the lease.
	defaultLeaseNS = "dpu-node-lease"
	// defaultLeaseZoneLabel is the label set on a lease that identifies the zone
	defaultLeaseZoneLabel = "k8s.ovn.org/node-lease-zone"
	// retryInterval is the interval between retries when updating or checking the lease
	retryInterval = 100 * time.Millisecond
	// retryNumber is the number of retries when updating or checking the lease
	retryNumber = 3
)

type heartbeatOptions struct {
	holderIdentity       string
	leaseDurationSeconds int32
	leaseNS              string
	mode                 string
	interval             time.Duration
}

type HeartbeatOption interface {
	Apply(*heartbeatOptions)
}

type HolderIdentityOption string

func (o HolderIdentityOption) Apply(options *heartbeatOptions) {
	options.holderIdentity = string(o)
}

type LeaseDurationSecondsOption int32

func (o LeaseDurationSecondsOption) Apply(options *heartbeatOptions) {
	options.leaseDurationSeconds = int32(o)
}

type LeaseNSOption string

func (o LeaseNSOption) Apply(options *heartbeatOptions) {
	options.leaseNS = string(o)
}

type ModeOption string

func (o ModeOption) Apply(options *heartbeatOptions) {
	options.mode = string(o)
}

type IntervalOption time.Duration

func (o IntervalOption) Apply(options *heartbeatOptions) {
	options.interval = time.Duration(o)
}

type heartbeat struct {
	nodeName  string
	zone      string
	client    kube.Interface
	clientSet kubernetes.Interface
	lease     *coordinationv1.Lease
	errChan   chan error
	heartbeatOptions
}

func makeOptions(opts ...HeartbeatOption) *heartbeatOptions {
	o := &heartbeatOptions{}
	for _, opt := range opts {
		opt.Apply(o)
	}

	if o.leaseDurationSeconds == 0 {
		o.leaseDurationSeconds = defaultLeaseDurationSeconds
	}
	if o.leaseNS == "" {
		o.leaseNS = defaultLeaseNS
	}
	if o.interval == 0 {
		// default interval is 10 seconds
		o.interval = 10 * time.Second
	}
	if o.mode == "" {
		o.mode = types.NodeModeDPU
	}
	return o
}

func newHeartbeat(client kube.Interface, clientSet kubernetes.Interface, nodeName, zone string, errChan chan error, opts ...HeartbeatOption) (*heartbeat, error) {
	o := makeOptions(opts...)

	if client == nil || clientSet == nil {
		return nil, fmt.Errorf("client and clienSet must be set")
	}

	return &heartbeat{
		nodeName:         nodeName,
		zone:             zone,
		client:           client,
		clientSet:        clientSet,
		errChan:          errChan,
		heartbeatOptions: *o,
	}, nil
}

func (h *heartbeat) run(ctx context.Context) error {
	switch h.mode {
	case types.NodeModeDPU:
		return h.runDPUNode(ctx)
	case types.NodeModeDPUHost:
		return h.runDPUHost(ctx)
	default:
		return fmt.Errorf("unknown node mode: %s", h.mode)
	}
}

func (h *heartbeat) runDPUNode(ctx context.Context) error {
	// check if lease exist
	lease, err := h.get(ctx)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

	}
	// if lease exit adopt and update
	if lease != nil {
		h.lease = lease
		if err = h.update(ctx, h.createLeaseSpec(h.lease.Spec.AcquireTime.Time, time.Now())); err != nil {
			return err
		}
	} else {
		// otherwise create it
		t := time.Now()
		if err = h.create(ctx, h.createLeaseSpec(t, t)); err != nil {
			return err
		}
	}

	go func() {
		ticker := newTicker(h.interval)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				// release the lease
				err := h.clientSet.CoordinationV1().Leases(h.leaseNS).Delete(ctx, h.nodeName, metav1.DeleteOptions{})
				h.errChan <- err
				return
			case <-ticker.C:
				if err := wait.ExponentialBackoffWithContext(ctx,
					wait.Backoff{
						Duration: retryInterval,
						Factor:   1.5,
						Steps:    retryNumber,
						Jitter:   0.4,
					}, func(context.Context) (done bool, err error) {
						if err = h.update(ctx, h.createLeaseSpec(h.lease.Spec.AcquireTime.Time, time.Now())); err != nil {
							klog.Errorf("Failed to update node lease for heartbeat: %v", err)
							return false, nil
						}
						return true, nil
					}); err != nil {
					// if canceled context, do not send error
					if errors.Is(err, context.Canceled) {
						h.errChan <- nil
						return
					}
					h.errChan <- fmt.Errorf("failed to update heartbeat lease: %w", err)
				}
			}
		}
	}()

	return nil
}

func (h *heartbeat) runDPUHost(ctx context.Context) error {
	go func() {
		ticker := newTicker(h.interval)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				h.errChan <- nil
				return
			case <-ticker.C:
				var errs []error
				if err := wait.ExponentialBackoffWithContext(ctx,
					wait.Backoff{
						Duration: retryInterval,
						Factor:   1.5,
						Steps:    retryNumber,
						Jitter:   0.4,
					}, func(context.Context) (done bool, err error) {
						if valid, err := isHeartBeatValid(ctx, h.clientSet, h.zone, h.leaseNS); err != nil || !valid {
							klog.Errorf("Heartbeat lease is not valid: %v", err)
							return false, nil
						}
						return true, nil
					}); err != nil {
					// if canceled context, do not send error
					if errors.Is(err, context.Canceled) {
						h.errChan <- nil
						return
					}
					errs = append(errs, fmt.Errorf("failed to check heartbeat lease: %w", err))
					if err := setNodeNetworkUnavailableTaint(ctx, h.client, h.nodeName); err != nil {
						klog.Errorf("Failed to set NetworkUnavailable taint: %v", err)
						errs = append(errs, err)
					}
					h.errChan <- kerrors.NewAggregate(errs)
					return
				}
			}
		}
	}()

	return nil
}

func (h *heartbeat) get(ctx context.Context) (*coordinationv1.Lease, error) {
	lease, err := h.clientSet.CoordinationV1().Leases(h.leaseNS).Get(ctx, h.nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (h *heartbeat) update(ctx context.Context, leaseSpec coordinationv1.LeaseSpec) error {
	if h.lease == nil {
		return errors.New("lease not initialized, call get or create first")
	}

	h.lease.Spec = leaseSpec

	lease, err := h.clientSet.CoordinationV1().Leases(h.leaseNS).Update(ctx, h.lease, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	h.lease = lease
	return nil
}

func (h *heartbeat) create(ctx context.Context, leaseSpec coordinationv1.LeaseSpec) error {
	var err error
	h.lease, err = h.clientSet.CoordinationV1().Leases(h.leaseNS).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.nodeName,
			Namespace: h.leaseNS,
			Labels: map[string]string{
				// this label sets the zone and will be used as label selector to find the lease
				defaultLeaseZoneLabel: h.zone,
			},
		},
		Spec: leaseSpec,
	}, metav1.CreateOptions{})
	return err
}

func (h *heartbeat) createLeaseSpec(acquireTime, renewTime time.Time) coordinationv1.LeaseSpec {
	return coordinationv1.LeaseSpec{
		HolderIdentity:       &h.holderIdentity,
		LeaseDurationSeconds: &h.leaseDurationSeconds,
		AcquireTime:          &metav1.MicroTime{Time: acquireTime},
		RenewTime:            &metav1.MicroTime{Time: renewTime},
	}
}

// isHeartBeatValid checks if there are any leases in the given namespace with the given zone label.
// If there are no leases, or if any lease is expired, it returns false.
// If all leases are valid, it returns true.
func isHeartBeatValid(ctx context.Context, client kubernetes.Interface, zone, ns string) (bool, error) {
	labelSelector := labels.Set{defaultLeaseZoneLabel: zone}.AsSelector()
	leases, err := client.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector.String(),
	})
	if err != nil {
		return false, err
	}

	if len(leases.Items) == 0 {
		return false, fmt.Errorf("no lease found in namespace %s", ns)
	}

	for _, lease := range leases.Items {
		if lease.Spec.RenewTime.Time.Add(time.Second * time.Duration(*lease.Spec.LeaseDurationSeconds)).Before(time.Now()) {
			return false, fmt.Errorf("lease %s is expired", lease.Name)
		}
	}

	return true, nil
}

func newTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}

// setNodeNetworkUnavailableTaint informs the Kubernetes scheduler and other controllers
// that the node should be avoided for pod placement unless the pod explicitly tolerates the taint.
// It does not evict already running pods.
func setNodeNetworkUnavailableTaint(ctx context.Context, client kube.Interface, nodeName string) error {
	// we use a well known taint node.kubernetes.io/network-unavailable, so that critical daemonsets can still tolerate it.
	// https://github.com/kubernetes/kubernetes/blob/f007012f5fe49e40ae0596cf463a8e7b247b3357/pkg/controller/daemon/util/daemonset_util.go#L95
	if err := wait.ExponentialBackoffWithContext(ctx,
		wait.Backoff{
			Duration: retryInterval,
			Factor:   1.5,
			Steps:    retryNumber,
			Jitter:   0.4,
		}, func(context.Context) (done bool, err error) {
			if err = client.SetTaintOnNode(nodeName, &kapi.Taint{
				Key:    kapi.TaintNodeNetworkUnavailable,
				Effect: kapi.TaintEffectNoSchedule,
			}); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
		return fmt.Errorf("failed to set NetworkUnavailable taint: %w", err)
	}
	return nil
}

func removeNodeNetworkUnavailableTaint(ctx context.Context, client kube.Interface, nodeName string) error {
	if err := wait.ExponentialBackoffWithContext(ctx,
		wait.Backoff{
			Duration: retryInterval,
			Factor:   1.5,
			Steps:    retryNumber,
			Jitter:   0.4,
		}, func(context.Context) (done bool, err error) {
			if err = client.RemoveTaintFromNode(nodeName, &kapi.Taint{
				Key:    kapi.TaintNodeNetworkUnavailable,
				Effect: kapi.TaintEffectNoSchedule,
			}); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
		return fmt.Errorf("failed to remove NetworkUnavailable taint: %w", err)
	}
	return nil
}
