package node

import (
	"context"
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler
	// stores the networkID of this network
	networkID *int
	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for the given secondary network. It supports layer3, layer2 and
// localnet topology types.
func NewSecondaryNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, netInfo util.NetInfo,
	vrfManager *vrfmanager.Controller, defaultNetworkGateway Gateway) (*SecondaryNodeNetworkController, error) {
	snnc := &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			NetInfo:                         netInfo,
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
		},
	}
	if util.IsNetworkSegmentationSupportEnabled() && snnc.IsPrimaryNetwork() {
		node, err := snnc.watchFactory.GetNode(snnc.name)
		if err != nil {
			return nil, fmt.Errorf("error retrieving node %s while creating node network controller for network %s: %v",
				snnc.name, netInfo.GetNetworkName(), err)
		}
		networkID, err := snnc.getNetworkID()
		if err != nil {
			return nil, fmt.Errorf("error retrieving network id for network %s: %v", netInfo.GetNetworkName(), err)
		}
		// FIXME (tssurya): Remove this match when L2 networks are supported
		if snnc.NetInfo.TopologyType() == types.Layer3Topology {
			snnc.gateway, err = NewUserDefinedNetworkGateway(snnc.NetInfo, networkID, node, snnc.watchFactory.NodeCoreInformer().Lister(), snnc.Kube, vrfManager, defaultNetworkGateway)
			if err != nil {
				return nil, fmt.Errorf("error creating UDN gateway for network %s: %v", netInfo.GetNetworkName(), err)
			}
		}
	}
	return snnc, nil
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())

	// enable adding ovs ports for dpu pods in both primary and secondary user defined networks
	if (config.OVNKubernetesFeature.EnableMultiNetwork || util.IsNetworkSegmentationSupportEnabled()) && config.OvnKubeNode.Mode == types.NodeModeDPU {
		handler, err := nc.watchPodsDPU()
		if err != nil {
			return err
		}
		nc.podHandler = handler
	}
	// FIXME (tssurya): Remove L3 match when L2 networks are supported
	if util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() && nc.TopologyType() == types.Layer3Topology {
		if err := nc.gateway.AddNetwork(); err != nil {
			return fmt.Errorf("failed to add network to node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}
	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	if nc.gateway != nil {
		return nc.gateway.DelNetwork()
	}
	return nil
}

func (oc *SecondaryNodeNetworkController) getNetworkID() (int, error) {
	if oc.networkID == nil || *oc.networkID == util.InvalidNetworkID {
		oc.networkID = ptr.To(util.InvalidNetworkID)
		nodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			return util.InvalidNetworkID, err
		}
		*oc.networkID, err = util.GetNetworkID(nodes, oc.NetInfo)
		if err != nil {
			return util.InvalidNetworkID, err
		}
	}
	return *oc.networkID, nil
}
