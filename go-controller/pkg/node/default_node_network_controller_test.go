package node

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	coordinationv1 "k8s.io/api/coordination/v1"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Node", func() {
	Describe("validateMTU", func() {
		var (
			kubeMock        *mocks.Interface
			netlinkOpsMock  *utilMocks.NetLinkOps
			netlinkLinkMock *netlink_mocks.Link

			nc *DefaultNodeNetworkController
		)

		const (
			nodeName  = "my-node"
			linkName  = "breth0"
			linkIPNet = "10.1.0.40/32"
			linkIndex = 4

			linkIPNet2 = "10.2.0.50/32"
			linkIndex2 = 5

			configDefaultMTU               = 1500 //value for config.Default.MTU
			mtuTooSmallForIPv4AndIPv6      = configDefaultMTU + types.GeneveHeaderLengthIPv4 - 1
			mtuOkForIPv4ButTooSmallForIPv6 = configDefaultMTU + types.GeneveHeaderLengthIPv4
			mtuOkForIPv4AndIPv6            = configDefaultMTU + types.GeneveHeaderLengthIPv6
			mtuTooSmallForSingleNode       = configDefaultMTU - 1
			mtuOkForSingleNode             = configDefaultMTU
		)

		BeforeEach(func() {
			kubeMock = new(mocks.Interface)
			netlinkOpsMock = new(utilMocks.NetLinkOps)
			netlinkLinkMock = new(netlink_mocks.Link)

			util.SetNetLinkOpMockInst(netlinkOpsMock)
			netlinkOpsMock.On("AddrList", nil, netlink.FAMILY_V4).
				Return([]netlink.Addr{
					{LinkIndex: linkIndex, IPNet: ovntest.MustParseIPNet(linkIPNet)},
					{LinkIndex: linkIndex2, IPNet: ovntest.MustParseIPNet(linkIPNet2)}}, nil)
			netlinkOpsMock.On("LinkByIndex", 4).Return(netlinkLinkMock, nil)

			nc = &DefaultNodeNetworkController{
				BaseNodeNetworkController: BaseNodeNetworkController{
					CommonNodeNetworkControllerInfo: CommonNodeNetworkControllerInfo{
						name: nodeName,
						Kube: kubeMock,
					},
					NetInfo: &util.DefaultNetInfo{},
				},
			}

			config.Default.MTU = configDefaultMTU
			config.Default.EncapIP = "10.1.0.40"

		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst() // other tests in this package rely directly on netlink (e.g. gateway_init_linux_test.go)
		})

		Context("with a cluster in IPv4 mode", func() {

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4ButTooSmallForIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a cluster in IPv6 mode", func() {
			BeforeEach(func() {
				config.IPv4Mode = false
				config.IPv6Mode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a cluster in dual-stack mode", func() {
			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4ButTooSmallForIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForIPv4AndIPv6,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with a single-node cluster", func() {
			BeforeEach(func() {
				config.Gateway.SingleNode = true
			})

			Context("with the node having a too small MTU", func() {

				It("should taint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuTooSmallForSingleNode,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).To(HaveOccurred())
				})
			})

			Context("with the node having a big enough MTU", func() {

				It("should untaint the node", func() {
					netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
						MTU:  mtuOkForSingleNode,
						Name: linkName,
					})

					err := nc.validateVTEPInterfaceMTU()
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("with multiple ovn encap IPs", func() {

			BeforeEach(func() {
				config.IPv4Mode = true
				config.IPv6Mode = false
				config.Default.EncapIP = "10.1.0.40,10.2.0.50"
				netlinkOpsMock.On("LinkByIndex", 5).Return(netlinkLinkMock, nil)
			})

			It("all interfaces have big enough MTU", func() {
				netlinkLinkMock.On("Attrs").Return(&netlink.LinkAttrs{
					MTU:  mtuOkForIPv4AndIPv6,
					Name: linkName,
				})

				err := nc.validateVTEPInterfaceMTU()
				Expect(err).NotTo(HaveOccurred())
			})
		})

	})

	Describe("Node Operations", func() {
		var app *cli.App

		BeforeEach(func() {
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())

			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags
		})

		It("sets correct OVN external IDs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "cannot.be.resolv.ed"
					interval int    = 100000
					ofintval int    = 180
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-openflow-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-is-interconn=false "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets non-default OVN encap port", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP      string = "1.2.5.6"
					nodeName    string = "cannot.be.resolv.ed"
					encapPort   uint   = 666
					interval    int    = 100000
					ofintval    int    = 180
					chassisUUID string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
					encapUUID   string = "e4437094-0094-4223-9f14-995d98d5fff8"
				)

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 " +
						"--if-exists get Open_vSwitch . external_ids:system-id"),
					Output: chassisUUID,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovn-sbctl --timeout=15 --no-leader-only --data=bare --no-heading --columns=_uuid find "+
						"Encap chassis_name=%s", chassisUUID),
					Output: encapUUID,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovn-sbctl --timeout=15 --no-leader-only set encap "+
						"%s options:dst_port=%d", encapUUID, encapPort),
				})

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Default.EncapPort = encapPort
				err = setEncapPort(context.Background())
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets non-default logical flow cache limits", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "cannot.be.resolv.ed"
					interval int    = 100000
					ofintval int    = 180
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-openflow-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-is-interconn=false "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=false "+
						"external_ids:ovn-set-local-ip=\"true\" "+
						"external_ids:ovn-limit-lflow-cache=1000 "+
						"external_ids:ovn-memlimit-lflow-cache-kb=100000",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				config.Default.LFlowCacheEnable = false
				config.Default.LFlowCacheLimit = 1000
				config.Default.LFlowCacheLimitKb = 100000
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("sets default IPFIX configuration", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP    string = "1.2.5.6"
					nodeName  string = "cannot.be.resolv.ed"
					interval  int    = 100000
					ofintval  int    = 180
					ipfixPort int32  = 456
				)
				ipfixIP := net.IP{1, 2, 3, 4}

				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-openflow-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-is-interconn=false "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15"+
						" -- "+
						"--id=@ipfix create ipfix "+
						"targets=[\"%s:%d\"] cache_active_timeout=60 sampling=400"+
						" -- "+
						"set bridge br-int ipfix=@ipfix", ipfixIP, ipfixPort),
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Monitoring.IPFIXTargets = []config.HostPort{
					{Host: &ipfixIP, Port: ipfixPort},
				}
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("allows overriding IPFIX configuration", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP    string = "1.2.5.6"
					nodeName  string = "cannot.be.resolv.ed"
					interval  int    = 100000
					ofintval  int    = 180
					ipfixPort int32  = 456
				)
				ipfixIP := net.IP{1, 2, 3, 4}

				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-openflow-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-is-interconn=false "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15"+
						" -- "+
						"--id=@ipfix create ipfix "+
						"targets=[\"%s:%d\"] cache_active_timeout=123 cache_max_flows=456 sampling=789"+
						" -- "+
						"set bridge br-int ipfix=@ipfix", ipfixIP, ipfixPort),
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())
				config.Monitoring.IPFIXTargets = []config.HostPort{
					{Host: &ipfixIP, Port: ipfixPort},
				}
				config.IPFIX.CacheActiveTimeout = 123
				config.IPFIX.CacheMaxFlows = 456
				config.IPFIX.Sampling = 789
				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("uses Node IP when the flow tracing targets only specify a port", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					nodeIP   string = "1.2.5.6"
					nodeName string = "anyhost.test"
					interval int    = 100000
					ofintval int    = 180
				)
				node := kapi.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: nodeIP,
							},
						},
					},
				}

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
						"external_ids:ovn-encap-type=geneve "+
						"external_ids:ovn-encap-ip=%s "+
						"external_ids:ovn-remote-probe-interval=%d "+
						"external_ids:ovn-openflow-probe-interval=%d "+
						"other_config:bundle-idle-timeout=%d "+
						"external_ids:hostname=\"%s\" "+
						"external_ids:ovn-is-interconn=false "+
						"external_ids:ovn-monitor-all=true "+
						"external_ids:ovn-ofctrl-wait-before-clear=0 "+
						"external_ids:ovn-enable-lflow-cache=true "+
						"external_ids:ovn-set-local-ip=\"true\"",
						nodeIP, interval, ofintval, ofintval, nodeName),
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15 -- clear bridge br-int netflow" +
						" -- " +
						"clear bridge br-int sflow" +
						" -- " +
						"clear bridge br-int ipfix",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-vsctl --timeout=15" +
						" -- " +
						"--id=@ipfix create ipfix " +
						// verify that the 1.2.5.6 IP has been attached to the :8888 target below
						`targets=["10.0.0.2:3030","1.2.5.6:8888","[2020:1111:f::1:933]:3333"] cache_active_timeout=60` +
						" -- " +
						"set bridge br-int ipfix=@ipfix",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				config.Monitoring.IPFIXTargets, err =
					config.ParseFlowCollectors("10.0.0.2:3030,:8888,[2020:1111:f::1:0933]:3333")
				config.IPFIX.CacheActiveTimeout = 60
				config.IPFIX.CacheMaxFlows = 0
				config.IPFIX.Sampling = 0
				Expect(err).NotTo(HaveOccurred())

				err = setupOVNNode(&node)
				Expect(err).NotTo(HaveOccurred())

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DPU Host Heartbeat", func() {
		const (
			uplinkName string = "enp3s0f0"
			hostIP     string = "192.168.1.10"
			hostCIDR   string = hostIP + "/24"
		)

		var (
			app *cli.App
		)

		BeforeEach(func() {
			app = cli.NewApp()
			app.Name = "test"
			app.Flags = config.Flags
		})

		It("should successfully check valid heartbeat", func() {
			heartbeatDPUHostTest(app, uplinkName, hostIP)
		})
	})
})

func heartbeatDPUHostTest(app *cli.App, uplinkName, hostIP string) {
	const (
		clusterCIDR string = "10.1.0.0/16"
		svcCIDR     string = "172.16.1.0/24"
		nodeName    string = "node1"
	)

	app.Action = func(ctx *cli.Context) error {
		fexec := ovntest.NewLooseCompareFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: hostIP}
		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient:             kubeFakeClient,
			AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		errChan := make(chan error)
		cnnci := NewCommonNodeNetworkControllerInfo(fakeClient.KubeClient, fakeClient.AdminPolicyRouteClient, nil, nil, nodeName, nil)
		nc := newDefaultNodeNetworkController(cnnci, stop, errChan, nil, nil)

		contx, cancel := context.WithCancel(context.Background())

		// check that the heartbeat fails when the lease is not created
		err = nc.checkDPUNodeHeartbeat(contx, config.Default.Zone, defaultLeaseNS, 10*time.Millisecond, 500*time.Millisecond)
		Expect(err).To(HaveOccurred())

		// simulate dpu node heartbeat
		nodeErrChan := make(chan error)
		nodeNC := newDefaultNodeNetworkController(NewCommonNodeNetworkControllerInfo(kubeFakeClient, nil, nil, nil, nodeName, nil), nil, nodeErrChan, nil, nil)
		err = nodeNC.startDPUNodeheartbeat(contx, config.Default.Zone, defaultLeaseNS, 1, 5*time.Millisecond)
		Expect(err).NotTo(HaveOccurred())

		// check that the heartbeat succeeds when the lease is created
		err = nc.checkDPUNodeHeartbeat(contx, config.Default.Zone, defaultLeaseNS, 10*time.Millisecond, 500*time.Millisecond)
		Expect(err).NotTo(HaveOccurred())

		// wait 1 second to ensure the lease is renewed
		time.Sleep(1 * time.Second)

		// cancel the context to stop the heartbeat
		cancel()

		//verify that no errors were reported
		err = <-errChan
		Expect(err).NotTo(HaveOccurred())

		err = <-nodeErrChan
		Expect(err).NotTo(HaveOccurred())

		// check that the node is not tainted
		node, err := kubeFakeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(node.Spec.Taints).To(HaveLen(0))

		// create a valid lease
		_, err = kubeFakeClient.CoordinationV1().Leases(defaultLeaseNS).Create(context.Background(), &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeName,
				Namespace: defaultLeaseNS,
				Labels: map[string]string{
					defaultLeaseZoneLabel: config.Default.Zone,
				},
			},
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: ptr.To(int32(40)),
				RenewTime:            &metav1.MicroTime{Time: time.Now()},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// launch the heartbeat again
		err = nc.checkDPUNodeHeartbeat(context.Background(), config.Default.Zone, defaultLeaseNS, 10*time.Millisecond, 500*time.Millisecond)
		Expect(err).NotTo(HaveOccurred())

		// update the lease to make it expire
		lease, err := kubeFakeClient.CoordinationV1().Leases(defaultLeaseNS).Get(context.Background(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-time.Hour)}
		_, err = kubeFakeClient.CoordinationV1().Leases(defaultLeaseNS).Update(context.Background(), lease, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			// check that the node is tainted
			node, err = kubeFakeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node.Spec.Taints).To(HaveLen(1))
			g.Expect(node.Spec.Taints[0].Key).To(Equal(kapi.TaintNodeNetworkUnavailable))
		}).ShouldNot(HaveOccurred())

		// update the lease to make it valid again
		lease, err = kubeFakeClient.CoordinationV1().Leases(defaultLeaseNS).Get(context.Background(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
		lease.Spec.LeaseDurationSeconds = ptr.To(int32(40))
		_, err = kubeFakeClient.CoordinationV1().Leases(defaultLeaseNS).Update(context.Background(), lease, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// check that the node is not tainted
		Eventually(func(g Gomega) {
			node, err = kubeFakeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node.Spec.Taints).To(HaveLen(0))
		}).ShouldNot(HaveOccurred())

		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + uplinkName,
		"--k8s-service-cidrs=" + svcCIDR,
		"--ovnkube-node-mode=dpu-host",
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}
