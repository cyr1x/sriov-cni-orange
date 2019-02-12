# SR-IOV CNI plugin

>> This is a fork of the Huscat sr-iov repository with added features <<

Here is a short description of SR-IOV CNI plugin and added features 

SR-IOV is an update of PCI Express specification to allow splitting a physical hardware resource (PF) into many virtual resources (VF) seen as lightweight PCIe functions. This is done by ensuring separation of its resource access. All is done in hardware, and this spec is not specific to network cards but is available as well for graphics cards and any PCIe card
Each SR-IOV compatible network card can generally be splitted into as many as 32 VF per physical port, and each VF can be given a dedicated VLAN (all network card parameters can't be configured per VF though). Some implementations allow setting a bandwidth limitation per VF as well, to deal with priority (for instance, a particular VF can be allowed a max bandwidth of 5Gbps, and all others limited to 1 Gbps)
As a result, a 2 port card can allow up to 64 containers to share the same 2 ports.

SR-IOV CNI is an open source extension that was developed to allow Kubernetes pods to gain access to SR-IOV acceleration and get near native network performance.

For NGPAAS, we forked this code to add several new features:
1.1.    Node annotations to help deal with available VFs
For simple Kubernetes installations (1 worker node with SR-IOV compatible card), it is easy to deal with available VFs, but as nodes increase in number and heterogeneity of hardware, it becomes painful to know which node owns free hardware resources in real time. More, when SR-IOV VFs are heavily used in a cluster, it results in unacceptable scheduling time because Kubernetes will attempt to find a node with free VF, but its standard scheduler is not aware of SR-IOV resources.
To help dealing with free VFs, we added code that maintains in real time 2 annotations for each worker node:
1.1.1.   sriov/vfCount 
This annotation is an integer that gives in real time the number of free VFs for a given node. It can be retrieved like any other annotation, and, for example, a custom scheduler extender can take advantage of this to exclude nodes without anymore free VF (Cf. our work on a scheduler extender)

1.1.2.   sriov/freeVFAvailable=true
This annotation is a label that only exists on worker nodes hosting free SR-IOV VFs. As a result, a pod requiring a SR-IOV network interface can specify its needs as easily as:
      nodeSelector:
        sriov/freeVFAvailable: true
1.2.     Pod new annotations to set useful VF properties
The current SR-IOV CNI doesn’t allow to set a VLAN nor a bandwidth limitation for a VF in a Kubernetes context. We added 2 annotations to be set in a pod definition yaml that allow its VF network interface to be automatically configured when the pod is created.
1.2.1.   VLAN
If a pod definition contains the following annotation: networks-sriov-vlan, its VF will be configured to send and receive packets tagged for a specified VLAN. This enables the use of VLANs for groups of Pods to separate traffic
1.2.2.   Outgoing Bandwidth limitation
If a pod definition contains the following annotation: networks-sriov-txrate, its VF will be throttled by the SR-IOV network card to allow at most the outgoing network bandwidth specified in the annotation (in Mbps). There is obviously no effect on the incoming packets.

----------------------------------


If you do not know CNI. Please read [here](https://github.com/containernetworking/cni) at first.

NIC with [SR-IOV](http://blog.scottlowe.org/2009/12/02/what-is-sr-iov/) capabilities works by introducing the idea of physical functions (PFs) and virtual functions (VFs). 

PF is used by host.Each VFs can be treated as a separate physical NIC and assigned to one container, and configured with separate MAC, VLAN and IP, etc.

## Build

This plugin requires Go 1.5+ to build.

Go 1.5 users will need to set `GO15VENDOREXPERIMENT=1` to get vendored dependencies. This flag is set by default in 1.6.

```
#./build
```

## Enable SR-IOV

Given Intel ixgbe NIC on CentOS, Fedora or RHEL:

```
# vi /etc/modprobe.conf
options ixgbe max_vfs=8,8
```

## Network configuration reference

* `name` (string, required): the name of the network
* `type` (string, required): "sriov"
* `master` (string, required): name of the PF
* `ipam` (dictionary, required): IPAM configuration to be used for this network.
* `pfOnly` (bool, optional): skip VFs, only assign PF into container

## Extra arguments

* `vf` (int, optional): VF index. This plugin will allocate a free VF if not assigned
* `vlan` (int, optional): VLAN ID for VF device
* `mac` (string, optional): mac address for VF device

## Usage

Given the following network configuration:

```
# cat > /etc/cni/net.d/10-mynet.conf <<EOF
{
    "name": "mynet",
    "type": "sriov",
    "master": "eth1",
    "ipam": {
        "type": "fixipam",
        "subnet": "10.55.206.0/26",
        "routes": [
            { "dst": "0.0.0.0/0" }
        ],
        "gateway": "10.55.206.1"
    }
}
EOF
```

Add container to network:

```sh
# CNI_PATH=`pwd`/bin
# cd scripts
# CNI_PATH=$CNI_PATH CNI_ARGS="IgnoreUnknown=1;IP=10.55.206.46;VF=1;MAC=66:d8:02:77:aa:aa" ./priv-net-run.sh ifconfig
contid=148e21a85bcc7aaf
netnspath=/var/run/netns/148e21a85bcc7aaf
eth0      Link encap:Ethernet  HWaddr 66:D8:02:77:AA:AA  
          inet addr:10.55.206.46  Bcast:0.0.0.0  Mask:255.255.255.192
          inet6 addr: fe80::64d8:2ff:fe77:aaaa/64 Scope:Link
          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1
          RX packets:0 errors:0 dropped:0 overruns:0 frame:0
          TX packets:7 errors:0 dropped:0 overruns:0 carrier:0
          collisions:0 txqueuelen:1000 
          RX bytes:0 (0.0 b)  TX bytes:558 (558.0 b)

lo        Link encap:Local Loopback  
          inet addr:127.0.0.1  Mask:255.0.0.0
          inet6 addr: ::1/128 Scope:Host
          UP LOOPBACK RUNNING  MTU:65536  Metric:1
          RX packets:0 errors:0 dropped:0 overruns:0 frame:0
          TX packets:0 errors:0 dropped:0 overruns:0 carrier:0
          collisions:0 txqueuelen:0 
          RX bytes:0 (0.0 b)  TX bytes:0 (0.0 b)
```

SRIOV VFs allow network SLAs. It's very useful.
And sometimes, we also need to occupy the entire NIC, such as vFirewall.
In this case, we can use pfOnly mode.

Create SRIOV network with PF mode.
Please see following as reference:
```
# cat > /etc/cni/net.d/10-mynet.conf <<EOF
{
    "name": "mynet",
    "type": "sriov",
    "master": "eth1",
    "pfOnly": true,
    "ipam": {
        "type": "fixipam",
        "subnet": "10.55.206.0/26",
        "routes": [
            { "dst": "0.0.0.0/0" }
        ],
        "gateway": "10.55.206.1"
    }
}
EOF
```

Add container to network:

```sh
# CNI_PATH=`pwd`/bin
# cd scripts
# CNI_PATH=$CNI_PATH CNI_ARGS="IgnoreUnknown=1;IP=10.55.206.46" ./priv-net-run.sh ifconfig
contid=148e21a85bcc7aaf
netnspath=/var/run/netns/148e21a85bcc7aaf
eth0: flags=4163<UP,BROADCAST,RUNNING,MULTICAST>  mtu 1500
        inet 10.55.206.46  netmask 255.255.255.192  broadcast 0.0.0.0
        inet6 fe80::215:5dff:fe38:101  prefixlen 64  scopeid 0x20<link>
        ether 00:15:5d:38:01:01  txqueuelen 1000  (Ethernet)
        RX packets 29  bytes 4960 (4.8 KiB)
        RX errors 0  dropped 0  overruns 0  frame 0
        TX packets 11  bytes 1398 (1.3 KiB)
        TX errors 0  dropped 0 overruns 0  carrier 0  collisions 0
```

Remove container from network:

```sh
# CNI_PATH=$CNI_PATH ./exec-plugins.sh del $contid /var/run/netns/$contid
```

For example:

```sh
# CNI_PATH=$CNI_PATH ./exec-plugins.sh del 148e21a85bcc7aaf /var/run/netns/148e21a85bcc7aaf
```

[More info](https://github.com/containernetworking/cni/pull/259).
