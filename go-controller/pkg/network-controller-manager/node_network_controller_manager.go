package networkControllerManager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
)

// nodeNetworkControllerManager structure is the object manages all controllers for all networks for ovnkube-node
type nodeNetworkControllerManager struct {
	name          string
	ovnNodeClient *util.OVNNodeClientset
	Kube          kube.Interface
	watchFactory  factory.NodeWatchFactory
	stopChan      chan struct{}
	errChan       chan error
	wg            *sync.WaitGroup
	recorder      record.EventRecorder

	defaultNodeNetworkController nad.BaseNetworkController

	// net-attach-def controller handle net-attach-def and create/delete secondary controllers
	// nil in dpu-host mode
	nadController *nad.NetAttachDefinitionController
	// vrf manager that creates and manages vrfs for all UDNs
	vrfManager *vrfmanager.Controller
	// route manager that creates and manages routes
	routeManager *routemanager.Controller
}

// NewNetworkController create secondary node network controllers for the given NetInfo
func (ncm *nodeNetworkControllerManager) NewNetworkController(nInfo util.NetInfo) (nad.NetworkController, error) {
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology, ovntypes.Layer2Topology, ovntypes.LocalnetTopology:
		dnnc, ok := ncm.defaultNodeNetworkController.(*node.DefaultNodeNetworkController)
		if !ok {
			return nil, fmt.Errorf("unable to deference default node network controller object")
		}
		return node.NewSecondaryNodeNetworkController(ncm.newCommonNetworkControllerInfo(), nInfo, ncm.vrfManager, dnnc.Gateway)
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// CleanupDeletedNetworks cleans up all stale entities giving list of all existing secondary network controllers
func (ncm *nodeNetworkControllerManager) CleanupDeletedNetworks(validNetworks ...util.BasicNetInfo) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}
	validVRFDevices := make(sets.Set[string])
	for _, network := range validNetworks {
		if !network.IsPrimaryNetwork() {
			continue
		}
		networkID, err := ncm.getNetworkID(network)
		if err != nil {
			klog.Errorf("Failed to get network identifier for network %s, error: %s", network.GetNetworkName(), err)
			continue
		}
		validVRFDevices.Insert(util.GetVRFDeviceNameForUDN(networkID))
	}
	return ncm.vrfManager.Repair(validVRFDevices)
}

func (ncm *nodeNetworkControllerManager) getNetworkID(network util.BasicNetInfo) (int, error) {
	nodes, err := ncm.watchFactory.GetNodes()
	if err != nil {
		return util.InvalidNetworkID, err
	}
	networkID, err := util.GetNetworkID(nodes, network)
	if err != nil {
		return util.InvalidNetworkID, err
	}
	return networkID, nil
}

// newCommonNetworkControllerInfo creates and returns the base node network controller info
func (ncm *nodeNetworkControllerManager) newCommonNetworkControllerInfo() *node.CommonNodeNetworkControllerInfo {
	return node.NewCommonNodeNetworkControllerInfo(ncm.ovnNodeClient.KubeClient, ncm.ovnNodeClient.AdminPolicyRouteClient, ncm.watchFactory, ncm.recorder, ncm.name, ncm.routeManager)
}

// NAD controller should be started on the node side under the following conditions:
// (1) dpu mode is enabled when secondary networks feature is enabled
// (2) primary user defined networks is enabled (all modes)
func isNodeNADControllerRequired() bool {
	return ((config.OVNKubernetesFeature.EnableMultiNetwork && config.OvnKubeNode.Mode == ovntypes.NodeModeDPU) ||
		util.IsNetworkSegmentationSupportEnabled())
}

// NewNodeNetworkControllerManager creates a new OVN controller manager to manage all the controller for all networks
func NewNodeNetworkControllerManager(ovnClient *util.OVNClientset, wf factory.NodeWatchFactory, name string,
	wg *sync.WaitGroup, eventRecorder record.EventRecorder, routeManager *routemanager.Controller, errChan chan error) (*nodeNetworkControllerManager, error) {
	ncm := &nodeNetworkControllerManager{
		name:          name,
		ovnNodeClient: &util.OVNNodeClientset{KubeClient: ovnClient.KubeClient, AdminPolicyRouteClient: ovnClient.AdminPolicyRouteClient},
		Kube:          &kube.Kube{KClient: ovnClient.KubeClient},
		watchFactory:  wf,
		stopChan:      make(chan struct{}),
		errChan:       errChan,
		wg:            wg,
		recorder:      eventRecorder,
		routeManager:  routeManager,
	}

	// need to configure OVS interfaces for Pods on secondary networks in the DPU mode
	// need to start NAD controller on node side for programming gateway pieces for UDNs
	var err error
	if isNodeNADControllerRequired() {
		ncm.nadController, err = nad.NewNetAttachDefinitionController("node-network-controller-manager", ncm, wf)
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		ncm.vrfManager = vrfmanager.NewController(ncm.routeManager)
	}
	if err != nil {
		return nil, err
	}
	return ncm, nil
}

// initDefaultNodeNetworkController creates the controller for default network
func (ncm *nodeNetworkControllerManager) initDefaultNodeNetworkController() error {
	defaultNodeNetworkController, err := node.NewDefaultNodeNetworkController(ncm.newCommonNetworkControllerInfo(), ncm.errChan)
	if err != nil {
		return err
	}
	// Make sure we only set defaultNodeNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	ncm.defaultNodeNetworkController = defaultNodeNetworkController
	return nil
}

// Start the node network controller manager
func (ncm *nodeNetworkControllerManager) Start(ctx context.Context) (err error) {
	klog.Infof("Starting the node network controller manager, Mode: %s", config.OvnKubeNode.Mode)

	// Initialize OVS exec runner; find OVS binaries that the CNI code uses.
	// Must happen before calling any OVS exec from pkg/cni to prevent races.
	// Not required in DPUHost mode as OVS is not present there.
	if err = cni.SetExec(kexec.New()); err != nil {
		return err
	}

	err = ncm.watchFactory.Start()
	if err != nil {
		return err
	}

	// make sure we clean up after ourselves on failure
	defer func() {
		if err != nil {
			ncm.Stop()
		}
	}()

	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost {
		// start health check to ensure there are no stale OVS internal ports
		go wait.Until(func() {
			if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
				checkForStaleOVSInternalPorts()
			}
			ncm.checkForStaleOVSRepresentorInterfaces()
		}, time.Minute, ncm.stopChan)
	}

	// Let's create Route manager that will manage routes.
	ncm.wg.Add(1)
	go func() {
		defer ncm.wg.Done()
		ncm.routeManager.Run(ncm.stopChan, 2*time.Minute)
	}()

	err = ncm.initDefaultNodeNetworkController()
	if err != nil {
		return fmt.Errorf("failed to init default node network controller: %v", err)
	}
	err = ncm.defaultNodeNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default node network controller: %v", err)
	}

	// nadController is nil if multi-network is disabled
	if ncm.nadController != nil {
		err = ncm.nadController.Start()
		if err != nil {
			return fmt.Errorf("failed to start NAD controller: %w", err)
		}
	}
	if ncm.vrfManager != nil {
		// Let's create VRF manager that will manage VRFs for all UDNs
		err = ncm.vrfManager.Run(ncm.stopChan, ncm.wg)
		if err != nil {
			return fmt.Errorf("failed to run VRF Manager: %w", err)
		}
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (ncm *nodeNetworkControllerManager) Stop() {
	// stop stale ovs ports cleanup
	close(ncm.stopChan)

	if ncm.defaultNodeNetworkController != nil {
		ncm.defaultNodeNetworkController.Stop()
	}

	// stop the NAD controller
	if ncm.nadController != nil {
		ncm.nadController.Stop()
	}
}

// checkForStaleOVSRepresentorInterfaces checks for stale OVS ports backed by Repreresentor interfaces,
// derive iface-id from pod name and namespace then remove any interfaces assoicated with a sandbox that are
// not scheduled to the node.
func (ncm *nodeNetworkControllerManager) checkForStaleOVSRepresentorInterfaces() {
	// Get all representor interfaces. these are OVS interfaces that have their external_ids:sandbox and vf-netdev-name set.
	out, stderr, err := util.RunOVSVsctl("--columns=name,external_ids", "--data=bare", "--no-headings",
		"--format=csv", "find", "Interface", "external_ids:sandbox!=\"\"", "external_ids:vf-netdev-name!=\"\"")
	if err != nil {
		klog.Errorf("Failed to list ovn-k8s OVS interfaces:, stderr: %q, error: %v", stderr, err)
		return
	}

	if out == "" {
		return
	}

	// parse this data into local struct
	type interfaceInfo struct {
		Name   string
		PodUID string
	}

	lines := strings.Split(out, "\n")
	interfaceInfos := make([]*interfaceInfo, 0, len(lines))
	for _, line := range lines {
		cols := strings.Split(line, ",")
		// Note: There are exactly 2 column entries as requested in the ovs query
		// Col 0: interface name
		// Col 1: space separated key=val pairs of external_ids attributes
		if len(cols) < 2 {
			// should never happen
			klog.Errorf("Unexpected output: %s, expect \"<name>,<external_ids>\"", line)
			continue
		}

		if cols[1] != "" {
			for _, attr := range strings.Split(cols[1], " ") {
				keyVal := strings.SplitN(attr, "=", 2)
				if len(keyVal) != 2 {
					// should never happen
					klog.Errorf("Unexpected output: %s, expect \"<key>=<value>\"", attr)
					continue
				} else if keyVal[0] == "iface-id-ver" {
					ifcInfo := interfaceInfo{Name: strings.TrimSpace(cols[0]), PodUID: keyVal[1]}
					interfaceInfos = append(interfaceInfos, &ifcInfo)
					break
				}
			}
		}
	}

	if len(interfaceInfos) == 0 {
		return
	}

	// list Pods and calculate the expected iface-ids.
	// Note: we do this after scanning ovs interfaces to avoid deleting ports of pods that where just scheduled
	// on the node.
	pods, err := ncm.watchFactory.GetPods("")
	if err != nil {
		klog.Errorf("Failed to list pods. %v", err)
		return
	}
	expectedPodUIDs := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Spec.NodeName == ncm.name && !util.PodWantsHostNetwork(pod) {
			// Note: wf (WatchFactory) *usually* returns pods assigned to this node, however we dont rely on it
			// and add this check to filter out pods assigned to other nodes. (e.g when ovnkube master and node
			// share the same process)
			expectedPodUIDs[string(pod.UID)] = struct{}{}
		}
	}

	// Remove any stale representor ports
	for _, ifaceInfo := range interfaceInfos {
		if _, ok := expectedPodUIDs[ifaceInfo.PodUID]; !ok {
			klog.Warningf("Found stale OVS Interface %s with iface-id-ver %s, deleting it", ifaceInfo.Name, ifaceInfo.PodUID)
			_, stderr, err := util.RunOVSVsctl("--if-exists", "--with-iface", "del-port", ifaceInfo.Name)
			if err != nil {
				klog.Errorf("Failed to delete interface %q . stderr: %q, error: %v",
					ifaceInfo.Name, stderr, err)
			}
		}
	}
}

// checkForStaleOVSInternalPorts checks for OVS internal ports without any ofport assigned,
// they are stale ports that must be deleted
func checkForStaleOVSInternalPorts() {
	// Track how long scrubbing stale interfaces takes
	start := time.Now()
	defer func() {
		klog.V(5).Infof("CheckForStaleOVSInternalPorts took %v", time.Since(start))
	}()

	stdout, _, err := util.RunOVSVsctl("--data=bare", "--no-headings", "--columns=name", "find",
		"interface", "ofport=-1")
	if err != nil {
		klog.Errorf("Failed to list OVS interfaces with ofport set to -1")
		return
	}
	if len(stdout) == 0 {
		return
	}
	// Batched command length overload shouldn't be a worry here since the number
	// of interfaces per node should never be very large
	// TODO: change this to use libovsdb
	staleInterfaceArgs := []string{}
	values := strings.Split(stdout, "\n\n")
	for _, val := range values {
		if val == ovntypes.K8sMgmtIntfName || val == ovntypes.K8sMgmtIntfName+"_0" {
			klog.Errorf("Management port %s is missing. Perhaps the host rebooted "+
				"or SR-IOV VFs were disabled on the host.", val)
			continue
		}
		klog.Warningf("Found stale interface %s, so queuing it to be deleted", val)
		if len(staleInterfaceArgs) > 0 {
			staleInterfaceArgs = append(staleInterfaceArgs, "--")
		}

		staleInterfaceArgs = append(staleInterfaceArgs, "--if-exists", "--with-iface", "del-port", val)
	}

	// Don't call ovs if all interfaces were skipped in the loop above
	if len(staleInterfaceArgs) == 0 {
		return
	}

	_, stderr, err := util.RunOVSVsctl(staleInterfaceArgs...)
	if err != nil {
		klog.Errorf("Failed to delete OVS port/interfaces: stderr: %s (%v)",
			stderr, err)
	}
}
