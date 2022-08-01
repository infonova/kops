/*
Copyright 2022 The Kubernetes Authors.

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

package hetznertasks

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/hetzner"
)

// +kops:fitask
type ServerGroup struct {
	Name      *string
	Lifecycle fi.Lifecycle
	SSHKeys   []*SSHKey
	Network   *Network

	Count      int
	NeedUpdate []string

	Location string
	Size     string
	Image    string

	EnableIPv4 bool
	EnableIPv6 bool

	UserData fi.Resource

	Labels map[string]string
}

func (v *ServerGroup) Find(c *fi.Context) (*ServerGroup, error) {
	cloud := c.Cloud.(hetzner.HetznerCloud)
	client := cloud.ServerClient()

	labelSelector := []string{
		fmt.Sprintf("%s=%s", hetzner.TagKubernetesClusterName, c.Cluster.Name),
		fmt.Sprintf("%s=%s", hetzner.TagKubernetesInstanceGroup, v.Labels[hetzner.TagKubernetesInstanceGroup]),
	}
	listOptions := hcloud.ListOpts{
		PerPage:       50,
		LabelSelector: strings.Join(labelSelector, ","),
	}
	serverListOptions := hcloud.ServerListOpts{ListOpts: listOptions}
	servers, err := client.AllWithOpts(context.TODO(), serverListOptions)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, nil
	}

	// Calculate the user-data hash
	userDataBytes, err := fi.ResourceAsBytes(v.UserData)
	if err != nil {
		return nil, err
	}
	userDataHash := safeBytesHash(userDataBytes)

	// Add the expected user-data hash label
	v.Labels[hetzner.TagKubernetesInstanceUserData] = userDataHash

	actual := *v
	actual.Count = len(servers)

	// Find servers that need to be updated
	for _, server := range servers {
		// Ignore servers that are already labeled as needing update
		if _, ok := server.Labels[hetzner.TagKubernetesInstanceNeedsUpdate]; ok {
			continue
		}

		// Check if server matches the expected group template
		if server.Labels[hetzner.TagKubernetesInstanceUserData] != userDataHash {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
		if server.Datacenter == nil || server.Datacenter.Location == nil || server.Datacenter.Location.Name != v.Location {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
		if server.ServerType == nil || server.ServerType.Name != v.Size {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
		if server.Image == nil || server.Image.Name != v.Image {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
		if (server.PublicNet.IPv4.IP != nil) != v.EnableIPv4 {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
		if (server.PublicNet.IPv6.IP != nil) != v.EnableIPv6 {
			actual.NeedUpdate = append(actual.NeedUpdate, server.Name)
			continue
		}
	}

	return &actual, nil
}

func (v *ServerGroup) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(v, c)
}

func (_ *ServerGroup) CheckChanges(a, e, changes *ServerGroup) error {
	if e.Name == nil {
		return fi.RequiredField("Name")
	}
	if e.Location == "" {
		return fi.RequiredField("Location")
	}
	if e.Size == "" {
		return fi.RequiredField("Size")
	}
	if e.Image == "" {
		return fi.RequiredField("Image")
	}
	if e.UserData == nil {
		return fi.RequiredField("UserData")
	}
	return nil
}

func (_ *ServerGroup) RenderHetzner(t *hetzner.HetznerAPITarget, a, e, changes *ServerGroup) error {
	client := t.Cloud.ServerClient()

	if a != nil {
		// Add "kops.k8s.io/needs-update" label to servers needing update
		for _, serverName := range a.NeedUpdate {
			server, _, err := client.GetByName(context.TODO(), serverName)
			if err != nil {
				return err
			}
			if server == nil {
				continue
			}

			server.Labels[hetzner.TagKubernetesInstanceNeedsUpdate] = ""
			_, _, err = client.Update(context.TODO(), server, hcloud.ServerUpdateOpts{
				Name:   server.Name,
				Labels: server.Labels,
			})
			if err != nil {
				return err
			}
		}
	}

	actualCount := 0
	if a != nil {
		actualCount = a.Count
	}
	expectedCount := e.Count

	if actualCount >= expectedCount {
		return nil
	}

	if len(e.SSHKeys) == 0 {
		return fmt.Errorf("failed to find ssh keys for server %q", fi.StringValue(e.Name))
	}
	if e.Network == nil {
		return fmt.Errorf("failed to find network for server %q", fi.StringValue(e.Name))
	}

	userData, err := fi.ResourceAsString(e.UserData)
	if err != nil {
		return err
	}
	userDataBytes, err := fi.ResourceAsBytes(e.UserData)
	if err != nil {
		return err
	}
	userDataHash := safeBytesHash(userDataBytes)

	for i := 1; i <= expectedCount-actualCount; i++ {
		// Append a random/unique ID to the node name
		name := fmt.Sprintf("%s-%x", e.Labels[hetzner.TagKubernetesInstanceGroup], rand.Int63())

		opts := hcloud.ServerCreateOpts{
			Name:             name,
			StartAfterCreate: fi.Bool(true),
			Networks: []*hcloud.Network{
				{
					ID: fi.IntValue(e.Network.ID),
				},
			},
			Location: &hcloud.Location{
				Name: e.Location,
			},
			ServerType: &hcloud.ServerType{
				Name: e.Size,
			},
			Image: &hcloud.Image{
				Name: e.Image,
			},
			UserData: userData,
			Labels:   e.Labels,
			PublicNet: &hcloud.ServerCreatePublicNet{
				EnableIPv4: e.EnableIPv4,
				EnableIPv6: e.EnableIPv6,
			},
		}

		// Add the SSH keys
		for _, sshkey := range e.SSHKeys {
			opts.SSHKeys = append(opts.SSHKeys, &hcloud.SSHKey{ID: fi.IntValue(sshkey.ID)})
		}

		// Add the user-data hash label
		opts.Labels[hetzner.TagKubernetesInstanceUserData] = userDataHash

		_, _, err = client.Create(context.TODO(), opts)
		if err != nil {
			return err
		}
	}

	return nil
}

func safeBytesHash(data []byte) string {
	// Calculate the SHA256 checksum of the data
	sum256 := sha256.Sum256(data)

	// Replace the unsupported chars with supported ones
	safe256 := base64.StdEncoding.EncodeToString(sum256[:])
	safe256 = strings.ReplaceAll(safe256, "+", "-")
	safe256 = strings.ReplaceAll(safe256, "/", "_")

	// Trim the unsupported "=" padding chars
	safe256 = strings.TrimRight(safe256, "=")

	return fmt.Sprintf("sha256.%s", safe256)
}
