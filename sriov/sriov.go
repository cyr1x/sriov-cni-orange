package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	//"encoding/json"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	netlink "github.com/vishvananda/netlink"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	//"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containernetworking/cni/pkg/types"

	. "github.com/hustcat/sriov-cni/config"
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

//functions to deal with k8s client

const ckubeconfig = "/etc/kubernetes/node-kubeconfig.yaml"
const cmachineid = "/etc/machine-id"
const cfreeVFAnnotation = "sriov/vfCount"
const cfreeVFLabel = "sriov/freeVFAvailable"
const cVLANAnnotation = "networks-sriov-vlan"
const cTXRate = "networks-sriov-txrate"

func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {

	// uses the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("createK8sClient: failed to get context for the kubeconfig %v, refer cyrsriov README.md for the usage guide %v", kubeconfig, err)
	}

	// creates the clientset
	return kubernetes.NewForConfig(config)
}

func logtmp(msg string) {

	f, _ := os.OpenFile("/tmp/dat.txt", os.O_APPEND|os.O_WRONLY, 0644)
	defer f.Close()
	_, _ = f.WriteString(msg)
	_, _ = f.WriteString("\n")
	f.Sync()

}

func getTotalVF(master string) (int, error) {

	sriovFile := fmt.Sprintf("/sys/class/net/%s/device/sriov_numvfs", master)
	if _, err := os.Lstat(sriovFile); err != nil {
		return -1, fmt.Errorf("failed to open the sriov_numfs of device %q: %v", master, err)
	}

	data, err := ioutil.ReadFile(sriovFile)
	if err != nil {
		return -1, fmt.Errorf("failed to read the sriov_numfs of device %q: %v", master, err)
	}

	if len(data) == 0 {
		return -1, fmt.Errorf("no data in the file %q", sriovFile)
	}

	sriovNumfs := strings.TrimSpace(string(data))
	vfTotal, err := strconv.Atoi(sriovNumfs)
	if err != nil {
		return -1, fmt.Errorf("failed to convert sriov_numfs(byte value) to int of device %q: %v", master, err)
	}

	if vfTotal <= 0 {
		return -1, fmt.Errorf("no virtual function in the device %q", master)
	}
	return vfTotal, nil

}

func getFreeVFs(master string, totalVFs int) int {

	numFreeVFs := 0

	for vf := 0; vf < totalVFs; vf++ {
		_, err := getVFDeviceName(master, vf)

		// got a free vf
		if err == nil {
			numFreeVFs++
		}
	}
	return numFreeVFs

}

func getCurrentNode(k8s *kubernetes.Clientset) (*v1.Node, error) {

	nodes, err := k8s.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("can't get node list: %v", err)
	}
	midbin, err := ioutil.ReadFile(cmachineid)
	if err != nil {
		return nil, fmt.Errorf("can't read kubernetes machine id: %v", err)
	}
	mid := strings.TrimRight(string(midbin), "\n")

	for _, node := range nodes.Items {
		nodeMid := node.Status.NodeInfo.MachineID

		if mid == nodeMid {
			//we found our node
			return &node, nil
		}
	}
	//shouldn'h happen
	return nil, fmt.Errorf("can't find working node")
}

func setNodeFreeVFsAnnot(k8s *kubernetes.Clientset, mynode *v1.Node, freeVFs int) error {

	annotations := mynode.GetObjectMeta().GetAnnotations()
	annotations[cfreeVFAnnotation] = strconv.Itoa(freeVFs)
	mynode.GetObjectMeta().SetAnnotations(annotations)
	_, err := k8s.CoreV1().Nodes().Update(mynode)
	if err != nil {
		return err
	}
	return nil
}

func setNodeFreeVFsLabel(k8s *kubernetes.Clientset, mynode *v1.Node, freeVFs int) error {

	labels := mynode.GetObjectMeta().GetLabels()
	stillFreeFVs := "false"
	if freeVFs > 0 {
		stillFreeFVs = "true"
	}
	labels[cfreeVFLabel] = stillFreeFVs
	mynode.GetObjectMeta().SetLabels(labels)
	_, err := k8s.CoreV1().Nodes().Update(mynode)
	if err != nil {
		return err
	}
	return nil
}

//from multus
// K8sArgs is the valid CNI_ARGS used for Kubernetes
type K8sArgs struct {
	types.CommonArgs
	IP                         net.IP
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

func getPodVLANAnnotation(k8s *kubernetes.Clientset, k K8sArgs) (string, error) {

	pod, err := k8s.CoreV1().Pods(string(k.K8S_POD_NAMESPACE)).Get(fmt.Sprintf("%s", string(k.K8S_POD_NAME)), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getPodVLANAnnotation: failed to query the pod %v in out of cluster comm", string(k.K8S_POD_NAME))
	}
	//using multus names

	return pod.Annotations[cVLANAnnotation], nil
}
func getPodTXRateAnnotation(k8s *kubernetes.Clientset, k K8sArgs) (string, error) {

	pod, err := k8s.CoreV1().Pods(string(k.K8S_POD_NAMESPACE)).Get(fmt.Sprintf("%s", string(k.K8S_POD_NAME)), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getPodVLANAnnotation: failed to query the pod %v in out of cluster comm", string(k.K8S_POD_NAME))
	}
	return pod.Annotations[cTXRate], nil
}

//end functions for k8s client

func setupPF(conf *SriovConf, ifName string, netns ns.NetNS) error {
	var (
		err error
	)

	masterName := conf.Net.Master
	args := conf.Args

	master, err := netlink.LinkByName(masterName)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", masterName, err)
	}

	if args.MAC != "" {
		return fmt.Errorf("modifying mac address of PF is not supported")
	}

	if args.VLAN != 0 {
		return fmt.Errorf("modifying vlan of PF is not supported")
	}

	if err = netlink.LinkSetUp(master); err != nil {
		return fmt.Errorf("failed to setup PF")
	}

	// move PF device to ns
	if err = netlink.LinkSetNsFd(master, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move PF to netns: %v", err)
	}

	return netns.Do(func(_ ns.NetNS) error {
		err := renameLink(masterName, ifName)
		if err != nil {
			return fmt.Errorf("failed to rename PF to %q: %v", ifName, err)
		}
		return nil
	})
}

func setupVF(conf *SriovConf, kconf K8sArgs, ifName string, netns ns.NetNS) error {
	var (
		err       error
		vfDevName string
	)

	vfIdx := 0
	masterName := conf.Net.Master
	args := conf.Args

	if args.VF != 0 {
		vfIdx = int(args.VF)
		vfDevName, err = getVFDeviceName(masterName, vfIdx)
		if err != nil {
			return err
		}
	} else {
		// alloc a free virtual function
		if vfIdx, vfDevName, err = allocFreeVF(masterName); err != nil {
			return err
		}
	}

	m, err := netlink.LinkByName(masterName)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", masterName, err)
	}

	vfDev, err := netlink.LinkByName(vfDevName)
	if err != nil {
		return fmt.Errorf("failed to lookup vf device %q: %v", vfDevName, err)
	}

	// set hardware address
	if args.MAC != "" {
		macAddr, err := net.ParseMAC(string(args.MAC))
		if err != nil {
			return err
		}
		if err = netlink.LinkSetVfHardwareAddr(m, vfIdx, macAddr); err != nil {
			return fmt.Errorf("failed to set vf %d macaddress: %v", vfIdx, err)
		}
	}

	k8s, err := createK8sClient(ckubeconfig)
	if err != nil {
		fmt.Println(fmt.Sprintf("%v", err))
		logtmp("createK8sClient failed")
	}

	//reset vlan/txrate
	err = netlink.LinkSetVfTxRate(m, vfIdx, 0)
	if err != nil {
		fmt.Println(fmt.Sprintf("%v", err))
	}
	err = netlink.LinkSetVfVlan(m, vfIdx, 0)
	if err != nil {
		fmt.Println(fmt.Sprintf("%v", err))
	}

	if args.VLAN != 0 {
		if err = netlink.LinkSetVfVlan(m, vfIdx, int(args.VLAN)); err != nil {
			return fmt.Errorf("failed to set vf %d vlan: %v", vfIdx, err)
		}
	}

	//sets vlan/txrate if right annotation set
	vlan, err := getPodVLANAnnotation(k8s, kconf)

	if err == nil && vlan != "" {
		logtmp("entre dans if")
		vlanint, err := strconv.Atoi(vlan)
		if err != nil {
			logtmp("getPodVLANAnnotation failed")
			return fmt.Errorf("failed to set vlan %s on vf %d device %q to %q: %v", vlan, vfIdx, vfDevName, ifName, err)
		}
		err = netlink.LinkSetVfVlan(m, vfIdx, vlanint)
		if err != nil {
			logtmp("LinkSetVfVlan failed")
			return fmt.Errorf("failed to set vlan %s on vf %d device %q to %q: %v", vlan, vfIdx, vfDevName, ifName, err)
		}

	}
	txrate, err := getPodTXRateAnnotation(k8s, kconf)
	logtmp(txrate)
	if err == nil && txrate != "" {
		txrateint, err := strconv.Atoi(txrate)
		if err != nil {
			logtmp("getPodTXRateAnnotation failed")
			return fmt.Errorf("failed to set txrate %s on vf %d device %q to %q: %v", txrate, vfIdx, vfDevName, ifName, err)
		}
		err = netlink.LinkSetVfTxRate(m, vfIdx, txrateint)
		if err != nil {
			logtmp("LinkSetVfTxRate failed")
			return fmt.Errorf("failed to set txrate %s on vf %d device %q to %q: %v", txrate, vfIdx, vfDevName, ifName, err)
		}

	}

	if err = netlink.LinkSetUp(vfDev); err != nil {
		return fmt.Errorf("failed to setup vf %d device: %v", vfIdx, err)
	}

	// move VF device to ns
	if err = netlink.LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move vf %d to netns: %v", vfIdx, err)
	}

	return netns.Do(func(_ ns.NetNS) error {
		err := renameLink(vfDevName, ifName)
		if err != nil {
			return fmt.Errorf("failed to rename vf %d device %q to %q: %v", vfIdx, vfDevName, ifName, err)
		}

		//compute free VFs available on node
		mynode, err := getCurrentNode(k8s)
		if err != nil {
			fmt.Println(fmt.Sprintf("%v", err))
		}
		nbTotVF, err := getTotalVF(masterName)
		if err != nil {
			fmt.Println(fmt.Sprintf("%v", err))
		}
		nbFreeVF := getFreeVFs(masterName, nbTotVF)
		errr := setNodeFreeVFsAnnot(k8s, mynode, nbFreeVF)
		if errr != nil {
			fmt.Println(fmt.Sprintf("%v", errr))
		}
		errr = setNodeFreeVFsLabel(k8s, mynode, nbFreeVF)
		if errr != nil {
			fmt.Println(fmt.Sprintf("%v", errr))
		}

		return nil
	})
}

func releasePF(conf *SriovConf, ifName string, netns ns.NetNS) error {
	initns, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to get init netns: %v", err)
	}

	// for IPAM in cmdDel
	return netns.Do(func(_ ns.NetNS) error {

		// get PF device
		master, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup device %s: %v", ifName, err)
		}

		masterName := conf.Net.Master

		// shutdown PF device
		if err = netlink.LinkSetDown(master); err != nil {
			return fmt.Errorf("failed to down device: %v", err)
		}

		// rename PF device
		err = renameLink(ifName, masterName)
		if err != nil {
			return fmt.Errorf("failed to rename device %s to %s: %v", ifName, masterName, err)
		}

		// move PF device to init netns
		if err = netlink.LinkSetNsFd(master, int(initns.Fd())); err != nil {
			return fmt.Errorf("failed to move device %s to init netns: %v", ifName, err)
		}

		return nil
	})
}

func releaseVF(conf *SriovConf, kconf K8sArgs, ifName string, netns ns.NetNS) error {
	initns, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to get init netns: %v", err)
	}

	// for IPAM in cmdDel
	return netns.Do(func(_ ns.NetNS) error {

		// get VF device
		vfDev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup device %s: %v", ifName, err)
		}

		// device name in init netns
		index := vfDev.Attrs().Index
		devName := fmt.Sprintf("dev%d", index)

		// shutdown VF device
		if err = netlink.LinkSetDown(vfDev); err != nil {
			return fmt.Errorf("failed to down device: %v", err)
		}

		// rename VF device
		err = renameLink(ifName, devName)
		if err != nil {
			return fmt.Errorf("failed to rename device %s to %s: %v", ifName, devName, err)
		}

		// move VF device to init netns
		if err = netlink.LinkSetNsFd(vfDev, int(initns.Fd())); err != nil {
			return fmt.Errorf("failed to move device %s to init netns: %v", ifName, err)
		}

		//compute free VFs available on node
		masterName := conf.Net.Master
		k8s, err := createK8sClient(ckubeconfig)
		if err != nil {
			fmt.Println(fmt.Sprintf("%v", err))
		}
		mynode, err := getCurrentNode(k8s)
		if err != nil {
			fmt.Println(fmt.Sprintf("%v", err))
		}
		nbTotVF, err := getTotalVF(masterName)
		if err != nil {
			fmt.Println(fmt.Sprintf("%v", err))
		}
		nbFreeVF := getFreeVFs(masterName, nbTotVF)
		errr := setNodeFreeVFsAnnot(k8s, mynode, nbFreeVF)
		if errr != nil {
			fmt.Println(fmt.Sprintf("%v", errr))
		}
		errr = setNodeFreeVFsLabel(k8s, mynode, nbFreeVF)
		if errr != nil {
			fmt.Println(fmt.Sprintf("%v", errr))
		}

		return nil
	})
}

func cmdAdd(args *skel.CmdArgs) error {

	n, err := LoadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	f, err := os.Create("/tmp/dat2")
	defer f.Close()
	_, err = f.WriteString(fmt.Sprintf("ContainerID: %v", args))
	f.Sync()

	kArgs := K8sArgs{}
	err = types.LoadArgs(args.Args, &kArgs)
	if err != nil {
		return fmt.Errorf("failed to open K8sArgs %q: %v", kArgs, err)
	}
	f2, err := os.Create("/tmp/dat3")
	defer f2.Close()
	_, err = f2.WriteString(fmt.Sprintf("k8s: %v", kArgs))
	f2.Sync()

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	if n.Net.PFOnly != true {
		if err = setupVF(n, kArgs, args.IfName, netns); err != nil {
			return err
		}
	} else {
		if err = setupPF(n, args.IfName, netns); err != nil {
			return err
		}
	}

	// run the IPAM plugin and get back the config to apply
	result, err := ipam.ExecAdd(n.Net.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}
	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}

	err = netns.Do(func(_ ns.NetNS) error {
		return ipam.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	result.DNS = n.Net.DNS
	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := LoadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}
	kArgs := K8sArgs{}
	err = types.LoadArgs(args.Args, &kArgs)
	if err != nil {
		return fmt.Errorf("failed to open K8sArgs %q: %v", kArgs, err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		// according to:
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		// if provided path does not exist (e.x. when node was restarted)
		// plugin should silently return with success after releasing
		// IPAM resources
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}

		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	if n.Net.PFOnly != true {
		if err = releaseVF(n, kArgs, args.IfName, netns); err != nil {
			return err
		}
	} else {
		if err = releasePF(n, args.IfName, netns); err != nil {
			return err
		}
	}

	err = ipam.ExecDel(n.Net.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	return nil
}

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		return err
	}

	return netlink.LinkSetName(link, newName)
}

func allocFreeVF(master string) (int, string, error) {
	vfIdx := -1
	devName := ""

	sriovFile := fmt.Sprintf("/sys/class/net/%s/device/sriov_numvfs", master)
	if _, err := os.Lstat(sriovFile); err != nil {
		return -1, "", fmt.Errorf("failed to open the sriov_numfs of device %q: %v", master, err)
	}

	data, err := ioutil.ReadFile(sriovFile)
	if err != nil {
		return -1, "", fmt.Errorf("failed to read the sriov_numfs of device %q: %v", master, err)
	}

	if len(data) == 0 {
		return -1, "", fmt.Errorf("no data in the file %q", sriovFile)
	}

	sriovNumfs := strings.TrimSpace(string(data))
	vfTotal, err := strconv.Atoi(sriovNumfs)
	if err != nil {
		return -1, "", fmt.Errorf("failed to convert sriov_numfs(byte value) to int of device %q: %v", master, err)
	}

	if vfTotal <= 0 {
		return -1, "", fmt.Errorf("no virtual function in the device %q", master)
	}

	for vf := 0; vf < vfTotal; vf++ {
		devName, err = getVFDeviceName(master, vf)

		// got a free vf
		if err == nil {
			vfIdx = vf
			break
		}
	}

	if vfIdx == -1 {
		return -1, "", fmt.Errorf("can not get a free virtual function in directory %s", master)
	}
	return vfIdx, devName, nil
}

func getVFDeviceName(master string, vf int) (string, error) {
	vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net", master, vf)
	if _, err := os.Lstat(vfDir); err != nil {
		return "", fmt.Errorf("failed to open the virtfn%d dir of the device %q: %v", vf, master, err)
	}

	infos, err := ioutil.ReadDir(vfDir)
	if err != nil {
		return "", fmt.Errorf("failed to read the virtfn%d dir of the device %q: %v", vf, master, err)
	}

	if len(infos) != 1 {
		return "", fmt.Errorf("no network device in directory %s", vfDir)
	}
	return infos[0].Name(), nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.Legacy)
}
