/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"fmt"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/servergroups"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/util/pkg/vfs"
)

func (c *openstackCloud) CreateServerGroup(opt servergroups.CreateOptsBuilder) (*servergroups.ServerGroup, error) {
	// TODO(sprietl): mutex + if server group exists -> return existing
	klog.Infof("CreateServerGroup: %v", opt.(servergroups.CreateOpts))
	c.mutex.Lock()
	defer c.mutex.Unlock()

	serverGroups, err := c.ListServerGroups(servergroups.ListOpts{})
	if err != nil {
		return nil, err
	}

	name := opt.(servergroups.CreateOpts).Name

	for _, sg := range serverGroups {
		if name == sg.Name {
			return &sg, nil
		}
	}

	return createServerGroup(c, opt)
}

func createServerGroup(c OpenstackCloud, opt servergroups.CreateOptsBuilder) (*servergroups.ServerGroup, error) {
	var i *servergroups.ServerGroup

	done, err := vfs.RetryWithBackoff(writeBackoff, func() (bool, error) {
		v, err := servergroups.Create(c.ComputeClient(), opt).Extract()
		if err != nil {
			return false, fmt.Errorf("error creating server group: %v", err)
		}
		i = v
		return true, nil
	})
	if err != nil {
		return i, err
	} else if done {
		return i, nil
	} else {
		return i, wait.ErrWaitTimeout
	}
}

func (c *openstackCloud) ListServerGroups(opts servergroups.ListOptsBuilder) ([]servergroups.ServerGroup, error) {
	return listServerGroups(c, opts)
}

func listServerGroups(c OpenstackCloud, opts servergroups.ListOptsBuilder) ([]servergroups.ServerGroup, error) {
	var sgs []servergroups.ServerGroup

	done, err := vfs.RetryWithBackoff(readBackoff, func() (bool, error) {
		allPages, err := servergroups.List(c.ComputeClient(), opts).AllPages()
		if err != nil {
			return false, fmt.Errorf("error listing server groups: %v", err)
		}

		r, err := servergroups.ExtractServerGroups(allPages)
		if err != nil {
			return false, fmt.Errorf("error extracting server groups from pages: %v", err)
		}
		sgs = r
		return true, nil
	})
	if err != nil {
		return sgs, err
	} else if done {
		return sgs, nil
	} else {
		return sgs, wait.ErrWaitTimeout
	}
}

// matchInstanceGroup filters a list of instancegroups for recognized cloud groups
func matchInstanceGroup(name string, clusterName string, instancegroups []*kops.InstanceGroup) ([]*kops.InstanceGroup, error) {
	var results []*kops.InstanceGroup

	for _, g := range instancegroups {
		var groupName string
		// var hasBackingSG bool
		var backingGroupName string

		switch g.Spec.Role {
		case kops.InstanceGroupRoleMaster, kops.InstanceGroupRoleNode, kops.InstanceGroupRoleBastion:
			groupName = clusterName + "-" + g.ObjectMeta.Name
			if v, ok := g.ObjectMeta.Annotations[OS_ANNOTATION+BACKING_SERVER_GROUP_NAME]; ok {
				backingGroupName = clusterName + "-" + v
			} else {
				backingGroupName = ""
			}

			// if v, ok := g.ObjectMeta.Annotations[OS_ANNOTATION+BACKING_SERVER_GROUP_NAME]; ok {
			// 	groupName = clusterName + "-" + v
			// 	hasBackingSG = true
			// } else {
			// 	groupName = clusterName + "-" + g.ObjectMeta.Name
			// 	hasBackingSG = false
			// }
		default:
			klog.Warningf("Ignoring InstanceGroup of unknown role %q", g.Spec.Role)
			continue
		}

		if name == groupName || name == backingGroupName {
			klog.Infof("matchInstanceGroup()::match for IG %s: %s == (%s || %s)", g.ObjectMeta.Name, name, groupName, backingGroupName)
			//if results != nil && !hasBackingSG {
			if results != nil && backingGroupName == "" {
				klog.Errorf("matchInstanceGroup()::found multiple instance groups matching servergrp %q", groupName)
				return nil, fmt.Errorf("found multiple instance groups matching servergrp %q", groupName)
			} else if results != nil && backingGroupName != "" {
				klog.Infof("matchInstanceGroup()::found multiple instance groups matching servergrp %q, allowing it due to backingSG %s", groupName, backingGroupName)
			}
			results = append(results, g)
		}
	}

	return results, nil
}

func osBuildCloudInstanceGroup(c OpenstackCloud, cluster *kops.Cluster, ig *kops.InstanceGroup, grps []servergroups.ServerGroup, nodeMap map[string]*v1.Node) (*cloudinstances.CloudInstanceGroup, error) {
	// newLaunchConfigName := g.Name
	newLaunchConfigName := cluster.ObjectMeta.Name + "-" + ig.ObjectMeta.Name
	cg := &cloudinstances.CloudInstanceGroup{
		HumanName:     newLaunchConfigName,
		InstanceGroup: ig,
		MinSize:       int(fi.Int32Value(ig.Spec.MinSize)),
		TargetSize:    int(fi.Int32Value(ig.Spec.MinSize)), // TODO: Retrieve the target size from OpenStack?
		MaxSize:       int(fi.Int32Value(ig.Spec.MaxSize)),
		Raw:           grps, // TODO: does it make sense to attach a slice here?
	}

	for _, g := range grps {
		klog.Infof("osBuildCloudInstanceGroup()::g.Members: %v", g.Members)
		for _, i := range g.Members {
			instanceId := i
			if instanceId == "" {
				klog.Warningf("ignoring instance with no instance id: %s", i)
				continue
			}
			server, err := servers.Get(c.ComputeClient(), instanceId).Extract()
			if err != nil {
				return nil, fmt.Errorf("Failed to get instance group member: %v", err)
			}

			actualIG := server.Metadata[TagKopsInstanceGroup]
			if actualIG != ig.ObjectMeta.Name {
				klog.Infof("ignoring instance %s (%s), which is not part of IG %s but is part of backingSG %s", server.Name, i, ig.ObjectMeta.Name, g.Name)
				continue
			}

			igObservedGeneration := server.Metadata[INSTANCE_GROUP_GENERATION]
			clusterObservedGeneration := server.Metadata[CLUSTER_GENERATION]
			observedName := fmt.Sprintf("%s-%s", clusterObservedGeneration, igObservedGeneration)
			generationName := fmt.Sprintf("%d-%d", cluster.GetGeneration(), ig.Generation)

			status := cloudinstances.CloudInstanceStatusUpToDate
			if generationName != observedName {
				status = cloudinstances.CloudInstanceStatusNeedsUpdate
			}
			cm, err := cg.NewCloudInstance(instanceId, status, nodeMap[instanceId])
			if err != nil {
				return nil, fmt.Errorf("error creating cloud instance group member: %v", err)
			}

			if server.Flavor["original_name"] != nil {
				cm.MachineType = server.Flavor["original_name"].(string)
			}

			ip, err := GetServerFixedIP(server, server.Metadata[TagKopsNetwork])
			if err != nil {
				return nil, fmt.Errorf("error creating cloud instance group member: %v", err)
			}

			cm.PrivateIP = ip

			cm.Roles = []string{server.Metadata["KopsRole"]}

		}
	}
	klog.Infof("osBuildCloudInstanceGroup()::cg: %+v", cg)
	return cg, nil
}

func (c *openstackCloud) DeleteServerGroup(groupID string) error {
	return deleteServerGroup(c, groupID)
}

func deleteServerGroup(c OpenstackCloud, groupID string) error {
	done, err := vfs.RetryWithBackoff(deleteBackoff, func() (bool, error) {
		err := servergroups.Delete(c.ComputeClient(), groupID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return false, fmt.Errorf("error deleting server group: %v", err)
		}
		if isNotFound(err) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	} else if done {
		return nil
	} else {
		return wait.ErrWaitTimeout
	}
}
