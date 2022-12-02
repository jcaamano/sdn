package master

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	kerrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"

	osdnv1 "github.com/openshift/api/network/v1"
	"github.com/openshift/sdn/pkg/network/common"
	masterutil "github.com/openshift/sdn/pkg/network/master/util"
)

func (master *OsdnMaster) startSubnetMaster() error {
	master.subnetAllocator = masterutil.NewSubnetAllocator()
	for _, cn := range master.networkInfo.ClusterNetworks {
		err := master.subnetAllocator.AddNetworkRange(cn.ClusterCIDR.String(), cn.HostSubnetLength)
		if err != nil {
			return err
		}
	}

	// Populate subnet allocator
	subnets, err := common.ListAllHostSubnets(context.TODO(), master.osdnClient)
	if err != nil {
		return err
	}
	for _, sn := range subnets {
		if err := master.subnetAllocator.MarkAllocatedNetwork(sn.Subnet); err != nil {
			klog.Errorf("Error marking allocated subnet: %v", err)
		}
	}

	master.watchNodes()
	master.watchSubnets()

	return nil
}

func (master *OsdnMaster) watchNodes() {
	funcs := common.InformerFuncs(&corev1.Node{}, master.handleAddOrUpdateNode, master.handleDeleteNode)
	master.nodeInformer.Informer().AddEventHandler(funcs)
}

func (master *OsdnMaster) handleAddOrUpdateNode(obj, _ interface{}, eventType watch.EventType) {
	node := obj.(*corev1.Node)

	nodeIP := common.GetNodeInternalIP(node)
	if len(nodeIP) == 0 {
		klog.Errorf("Node IP is not set for node %s, skipping %s event, node: %v", node.Name, eventType, node)
		return
	}

	master.clearInitialNodeNetworkUnavailableCondition(node)

	if oldNodeIP, ok := master.hostSubnetNodeIPs[node.UID]; ok && (nodeIP == oldNodeIP) {
		return
	}
	// Node status is frequently updated by kubelet, so log only if the above condition is not met
	klog.V(5).Infof("Watch %s event for Node %q", eventType, node.Name)

	err := master.addNode(node.Name, string(node.UID), nodeIP, nil)
	if err != nil {
		klog.Errorf("Error creating subnet for node %s, ip %s: %v", node.Name, nodeIP, err)
		return
	}
	master.hostSubnetNodeIPs[node.UID] = nodeIP
}

func (master *OsdnMaster) handleDeleteNode(obj interface{}) {
	node := obj.(*corev1.Node)
	klog.V(5).Infof("Watch %s event for Node %q", watch.Deleted, node.Name)

	if _, exists := master.hostSubnetNodeIPs[node.UID]; !exists {
		return
	}

	delete(master.hostSubnetNodeIPs, node.UID)

	if err := master.deleteNode(node.Name); err != nil {
		klog.Errorf("Error deleting node %s: %v", node.Name, err)
		return
	}
}

// addNode takes the nodeName, a preferred nodeIP and the node's annotations
// Creates or updates a HostSubnet if needed
func (master *OsdnMaster) addNode(nodeName string, nodeUID string, nodeIP string, hsAnnotations map[string]string) error {
	// Validate node IP before proceeding
	if err := master.networkInfo.ValidateNodeIP(nodeIP); err != nil {
		return err
	}

	// Check if subnet needs to be created or updated
	sub, err := master.osdnClient.NetworkV1().HostSubnets().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err == nil {
		if err = common.ValidateHostSubnet(sub); err != nil {
			klog.Errorf("Deleting invalid HostSubnet %q: %v", nodeName, err)
			_ = master.osdnClient.NetworkV1().HostSubnets().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
			// fall through to create new subnet below
		} else if sub.HostIP == nodeIP {
			return nil
		} else {
			// Node IP changed, update old subnet
			sub.HostIP = nodeIP
			sub, err = master.osdnClient.NetworkV1().HostSubnets().Update(context.TODO(), sub, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("error updating subnet %s for node %s: %v", sub.Subnet, nodeName, err)
			}
			klog.Infof("Updated HostSubnet %s", common.HostSubnetToString(sub))
			return nil
		}
	}

	// Create new subnet
	if len(nodeUID) != 0 {
		if hsAnnotations == nil {
			hsAnnotations = make(map[string]string)
		}
		hsAnnotations[osdnv1.NodeUIDAnnotation] = nodeUID
	}
	network, err := master.subnetAllocator.AllocateNetwork()
	if err != nil {
		return fmt.Errorf("error allocating network for node %s: %v", nodeName, err)
	}
	sub = &osdnv1.HostSubnet{
		TypeMeta:   metav1.TypeMeta{Kind: "HostSubnet"},
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: hsAnnotations},
		Host:       nodeName,
		HostIP:     nodeIP,
		Subnet:     network,
	}
	sub, err = master.osdnClient.NetworkV1().HostSubnets().Create(context.TODO(), sub, metav1.CreateOptions{})
	if err != nil {
		if er := master.subnetAllocator.ReleaseNetwork(network); er != nil {
			klog.Errorf("Error releasing allocated subnet: %v", err)
		}
		return fmt.Errorf("error allocating subnet for node %q: %v", nodeName, err)
	}
	klog.Infof("Created HostSubnet %s", common.HostSubnetToString(sub))
	return nil
}

func (master *OsdnMaster) deleteNode(nodeName string) error {
	subInfo := nodeName
	// If create and delete events for the same node are called in quick succession,
	// hostsubnet informer cache may not have corresponding item. We fetch the object just for logging.
	// So if we get the object we will log in detail otherwise will log in brief.
	if sub, err := master.hostSubnetInformer.Lister().Get(nodeName); err == nil {
		subInfo = common.HostSubnetToString(sub)
	}
	if err := master.osdnClient.NetworkV1().HostSubnets().Delete(context.TODO(), nodeName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("error deleting subnet for node %q: %v", nodeName, err)
	}

	klog.Infof("Deleted HostSubnet %s", subInfo)
	return nil
}

// Because openshift-sdn uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (master *OsdnMaster) clearInitialNodeNetworkUnavailableCondition(origNode *corev1.Node) {
	// Informer cache should not be mutated, so get a copy of the object
	node := origNode.DeepCopy()
	knode := node
	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		if knode != node {
			knode, err = master.nodeInformer.Lister().Get(node.Name)
			if err != nil {
				return err
			}
		}

		for i := range knode.Status.Conditions {
			if knode.Status.Conditions[i].Type == corev1.NodeNetworkUnavailable {
				condition := &knode.Status.Conditions[i]
				if condition.Status != corev1.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = corev1.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "openshift-sdn cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()

					if knode, err = master.kClient.CoreV1().Nodes().UpdateStatus(context.TODO(), knode, metav1.UpdateOptions{}); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		klog.Errorf("Status update failed for local node: %v", resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", node.Name)
	}
}

func (master *OsdnMaster) watchSubnets() {
	funcs := common.InformerFuncs(&osdnv1.HostSubnet{}, master.handleAddOrUpdateSubnet, master.handleDeleteSubnet)
	master.hostSubnetInformer.Informer().AddEventHandler(funcs)
}

func (master *OsdnMaster) handleAddOrUpdateSubnet(obj, _ interface{}, eventType watch.EventType) {
	hs := obj.(*osdnv1.HostSubnet)
	klog.V(5).Infof("Watch %s event for HostSubnet %q", eventType, hs.Name)

	if err := common.ValidateHostSubnet(hs); err != nil {
		klog.Errorf("Ignoring invalid HostSubnet %s: %v", common.HostSubnetToString(hs), err)
		return
	}

	if err := master.reconcileHostSubnet(hs); err != nil {
		klog.Errorf("Error reconciling HostSubnet: %v", err)
	}
	if err := master.networkInfo.ValidateNodeIP(hs.HostIP); err != nil {
		// Don't error out; just warn so the error can be corrected with 'oc'
		klog.Errorf("Failed to validate HostSubnet %s: %v", common.HostSubnetToString(hs), err)
	}

	if _, ok := hs.Annotations[osdnv1.AssignHostSubnetAnnotation]; ok {
		if err := master.handleAssignHostSubnetAnnotation(hs); err != nil {
			klog.Errorf("Error handling AssignHostSubnetAnnotation: %v", err)
			return
		}
	}
}

func (master *OsdnMaster) handleDeleteSubnet(obj interface{}) {
	hs := obj.(*osdnv1.HostSubnet)
	klog.V(5).Infof("Watch %s event for HostSubnet %q", watch.Deleted, hs.Name)

	if _, ok := hs.Annotations[osdnv1.AssignHostSubnetAnnotation]; ok {
		return
	}

	if err := master.subnetAllocator.ReleaseNetwork(hs.Subnet); err != nil {
		klog.Errorf("Error releasing allocated subnet: %v", err)
	}
}

// reconcileHostSubnet verifies and corrects the state of the hostsubnet.
// Because openshift watches on events to keep hostsubnets and nodes in the correct state, missing an event
// can cause orphaned or unusable hostsubnets to stick around.
func (master *OsdnMaster) reconcileHostSubnet(subnet *osdnv1.HostSubnet) error {
	var node *corev1.Node
	var err error
	node, err = master.nodeInformer.Lister().Get(subnet.Name)
	if err != nil {
		node, err = master.kClient.CoreV1().Nodes().Get(context.TODO(), subnet.Name, metav1.GetOptions{})
		if err != nil {
			if kerrs.IsNotFound(err) {
				node = nil
			} else {
				return fmt.Errorf("error fetching node for subnet %q: %v", subnet.Name, err)
			}
		}
	}

	if node == nil && len(subnet.Annotations[osdnv1.NodeUIDAnnotation]) == 0 {
		// Subnet belongs to F5, Ignore.
		return nil
	} else if node != nil && len(subnet.Annotations[osdnv1.NodeUIDAnnotation]) == 0 {
		// Update path, stamp UID annotation on subnet.
		sn := subnet.DeepCopy()
		if sn.Annotations == nil {
			sn.Annotations = make(map[string]string)
		}
		sn.Annotations[osdnv1.NodeUIDAnnotation] = string(node.UID)
		if _, err = master.osdnClient.NetworkV1().HostSubnets().Update(context.TODO(), sn, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error updating subnet %v for node %s: %v", sn, sn.Name, err)
		}
	} else if node == nil && len(subnet.Annotations[osdnv1.NodeUIDAnnotation]) > 0 {
		// Missed Node event, delete stale subnet.
		klog.Infof("Setup found no node associated with hostsubnet %s, deleting the hostsubnet", subnet.Name)
		if err = master.osdnClient.NetworkV1().HostSubnets().Delete(context.TODO(), subnet.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("error deleting subnet %v: %v", subnet, err)
		}
	} else if string(node.UID) != subnet.Annotations[osdnv1.NodeUIDAnnotation] {
		// Missed Node event, node with the same name exists delete stale subnet.
		klog.Infof("Missed node event, hostsubnet %s has the UID of an incorrect object, deleting the hostsubnet", subnet.Name)
		if err = master.osdnClient.NetworkV1().HostSubnets().Delete(context.TODO(), subnet.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("error deleting subnet %v: %v", subnet, err)
		}
	}
	return nil
}

// Handle F5 use case: Admin manually creates HostSubnet with 'AssignHostSubnetAnnotation'
// to allocate a subnet with no real node in the cluster.
func (master *OsdnMaster) handleAssignHostSubnetAnnotation(hs *osdnv1.HostSubnet) error {
	// Delete the annotated hostsubnet and create a new one with an assigned subnet
	// We do not update (instead of delete+create) because the watchSubnets on the nodes
	// will skip the event if it finds that the hostsubnet has the same host
	// And we cannot fix the watchSubnets code for node because it will break migration if
	// nodes are upgraded after the master
	if err := master.osdnClient.NetworkV1().HostSubnets().Delete(context.TODO(), hs.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("error in deleting annotated subnet: %s, %v", hs.Name, err)
	}
	klog.Infof("Deleted HostSubnet not backed by node: %s", common.HostSubnetToString(hs))

	var hsAnnotations map[string]string
	if vnid, ok := hs.Annotations[osdnv1.FixedVNIDHostAnnotation]; ok {
		vnidInt, err := strconv.Atoi(vnid)
		if err == nil && vnidInt >= 0 && uint32(vnidInt) <= common.MaxVNID {
			hsAnnotations = make(map[string]string)
			hsAnnotations[osdnv1.FixedVNIDHostAnnotation] = strconv.Itoa(vnidInt)
		} else {
			klog.Errorf("VNID %s is an invalid value for annotation %s. Annotation will be ignored.", vnid, osdnv1.FixedVNIDHostAnnotation)
		}
	}

	if err := master.addNode(hs.Name, "", hs.HostIP, hsAnnotations); err != nil {
		return fmt.Errorf("error creating subnet: %s, %v", hs.Name, err)
	}
	klog.Infof("Created HostSubnet not backed by node: %s", common.HostSubnetToString(hs))
	return nil
}
