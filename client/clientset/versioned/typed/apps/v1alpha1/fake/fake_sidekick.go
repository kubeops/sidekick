/*
Copyright AppsCode Inc. and Contributors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeSidekicks implements SidekickInterface
type FakeSidekicks struct {
	Fake *FakeAppsV1alpha1
	ns   string
}

var sidekicksResource = schema.GroupVersionResource{Group: "apps.k8s.appscode.com", Version: "v1alpha1", Resource: "sidekicks"}

var sidekicksKind = schema.GroupVersionKind{Group: "apps.k8s.appscode.com", Version: "v1alpha1", Kind: "Sidekick"}

// Get takes name of the sidekick, and returns the corresponding sidekick object, and an error if there is any.
func (c *FakeSidekicks) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.Sidekick, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(sidekicksResource, c.ns, name), &v1alpha1.Sidekick{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sidekick), err
}

// List takes label and field selectors, and returns the list of Sidekicks that match those selectors.
func (c *FakeSidekicks) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.SidekickList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(sidekicksResource, sidekicksKind, c.ns, opts), &v1alpha1.SidekickList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.SidekickList{ListMeta: obj.(*v1alpha1.SidekickList).ListMeta}
	for _, item := range obj.(*v1alpha1.SidekickList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested sidekicks.
func (c *FakeSidekicks) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(sidekicksResource, c.ns, opts))

}

// Create takes the representation of a sidekick and creates it.  Returns the server's representation of the sidekick, and an error, if there is any.
func (c *FakeSidekicks) Create(ctx context.Context, sidekick *v1alpha1.Sidekick, opts v1.CreateOptions) (result *v1alpha1.Sidekick, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(sidekicksResource, c.ns, sidekick), &v1alpha1.Sidekick{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sidekick), err
}

// Update takes the representation of a sidekick and updates it. Returns the server's representation of the sidekick, and an error, if there is any.
func (c *FakeSidekicks) Update(ctx context.Context, sidekick *v1alpha1.Sidekick, opts v1.UpdateOptions) (result *v1alpha1.Sidekick, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(sidekicksResource, c.ns, sidekick), &v1alpha1.Sidekick{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sidekick), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeSidekicks) UpdateStatus(ctx context.Context, sidekick *v1alpha1.Sidekick, opts v1.UpdateOptions) (*v1alpha1.Sidekick, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(sidekicksResource, "status", c.ns, sidekick), &v1alpha1.Sidekick{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sidekick), err
}

// Delete takes name of the sidekick and deletes it. Returns an error if one occurs.
func (c *FakeSidekicks) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(sidekicksResource, c.ns, name, opts), &v1alpha1.Sidekick{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeSidekicks) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(sidekicksResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.SidekickList{})
	return err
}

// Patch applies the patch and returns the patched sidekick.
func (c *FakeSidekicks) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.Sidekick, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(sidekicksResource, c.ns, name, pt, data, subresources...), &v1alpha1.Sidekick{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sidekick), err
}