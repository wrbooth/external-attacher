/*
Copyright 2017 The Kubernetes Authors.

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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	"github.com/kubernetes-csi/external-attacher/pkg/connection"

	"k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	csiapi "k8s.io/csi-api/pkg/apis/csi/v1alpha1"
	fakecsi "k8s.io/csi-api/pkg/client/clientset/versioned/fake"
	csiinformers "k8s.io/csi-api/pkg/client/informers/externalversions"
)

// This is an unit test framework. It is heavily inspired by serviceaccount
// controller tests.

type reaction struct {
	verb     string
	resource string
	reactor  func(t *testing.T) core.ReactionFunc
}

type testCase struct {
	// Name of the test (for logging)
	name string
	// Object to insert into fake kubeclient before the test starts.
	initialObjects []runtime.Object
	// Optional client reactors
	reactors []reaction
	// Optional VolumeAttachment that's used to simulate "VA added" event.
	// This VA is also automatically added to initialObjects.
	addedVA *storage.VolumeAttachment
	// Optional VolumeAttachment that's used to simulate "VA updated" event.
	// This VA is also automatically added to initialObjects.
	updatedVA *storage.VolumeAttachment
	// Optional VolumeAttachment that's used to simulate "VA deleted" event.
	deletedVA *storage.VolumeAttachment
	// Optional {V} that's used to simulate "PV updated" event.
	// This PV is also automatically added to initialObjects.
	updatedPV *v1.PersistentVolume
	// List of expected kubeclient actions that should happen during the test.
	expectedActions []core.Action
	// List of expected CSI calls
	expectedCSICalls []csiCall
	// Function to perform additional checks after the test finishes
	additionalCheck func(t *testing.T, test testCase)
}

type csiCall struct {
	// Name that's supposed to be called. "attach" or "detach". Other CSI calls
	// are not supported for testing.
	functionName string
	// Expected volume handle
	volumeHandle string
	// Expected CSI's ID of the node
	nodeID string
	// Expected volume attributes
	volumeAttributes map[string]string
	// Expected secrets
	secrets map[string]string
	// error to return
	err error
	// "detached" bool to return. Used only when err != nil
	detached bool
	// metadata to return (used only in Attach calls)
	metadata map[string]string
	// Force the attach or detach to take a certain amount of time
	delay time.Duration
}

type handlerFactory func(client kubernetes.Interface, informerFactory informers.SharedInformerFactory, csiInformerFactory csiinformers.SharedInformerFactory, csi connection.CSIConnection) Handler

func runTests(t *testing.T, handlerFactory handlerFactory, tests []testCase) {
	for _, test := range tests {
		glog.Infof("Test %q: started", test.name)
		objs := test.initialObjects
		if test.addedVA != nil {
			objs = append(objs, test.addedVA)
		}
		if test.updatedVA != nil {
			objs = append(objs, test.updatedVA)
		}
		if test.updatedPV != nil {
			objs = append(objs, test.updatedPV)
		}

		coreObjs := []runtime.Object{}
		csiObjs := []runtime.Object{}
		for _, obj := range objs {
			switch obj.(type) {
			case *csiapi.CSINodeInfo:
				csiObjs = append(csiObjs, obj)
			default:
				coreObjs = append(coreObjs, obj)
			}
		}

		// Create client and informers
		client := fake.NewSimpleClientset(coreObjs...)
		csiClient := fakecsi.NewSimpleClientset(csiObjs...)
		informers := informers.NewSharedInformerFactory(client, time.Hour /* disable resync*/)
		vaInformer := informers.Storage().V1beta1().VolumeAttachments()
		pvInformer := informers.Core().V1().PersistentVolumes()
		nodeInformer := informers.Core().V1().Nodes()
		csiInformers := csiinformers.NewSharedInformerFactory(csiClient, time.Hour /* disable resync*/)
		nodeInfoInformer := csiInformers.Csi().V1alpha1().CSINodeInfos()
		// Fill the informers with initial objects so controller can Get() them
		for _, obj := range objs {
			switch obj.(type) {
			case *v1.PersistentVolume:
				pvInformer.Informer().GetStore().Add(obj)
			case *v1.Node:
				nodeInformer.Informer().GetStore().Add(obj)
			case *storage.VolumeAttachment:
				vaInformer.Informer().GetStore().Add(obj)
			case *v1.Secret:
				// Secrets are not cached in any informer
			case *csiapi.CSINodeInfo:
				nodeInfoInformer.Informer().GetStore().Add(obj)
			default:
				t.Fatalf("Unknown initalObject type: %+v", obj)
			}
		}
		// This reactor makes sure that all updates that the controller does are
		// reflected in its informers so Lister.Get() finds them. This does not
		// enqueue events!
		client.Fake.PrependReactor("update", "*", func(action core.Action) (bool, runtime.Object, error) {
			if action.GetVerb() == "update" {
				switch action.GetResource().Resource {
				case "volumeattachments":
					glog.V(5).Infof("Test reactor: updated VA")
					vaInformer.Informer().GetStore().Update(action.(core.UpdateAction).GetObject())
				case "persistentvolumes":
					glog.V(5).Infof("Test reactor: updated PV")
					pvInformer.Informer().GetStore().Update(action.(core.UpdateAction).GetObject())
				default:
					t.Errorf("Unknown update resource: %s", action.GetResource())
				}
			}
			return false, nil, nil
		})
		// Run any reactors that the test needs *before* the above one.
		for _, reactor := range test.reactors {
			client.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactor(t))
		}

		// Construct controller
		csiConnection := &fakeCSIConnection{t: t, calls: test.expectedCSICalls}
		handler := handlerFactory(client, informers, csiInformers, csiConnection)
		ctrl := NewCSIAttachController(client, testAttacherName, handler, vaInformer, pvInformer)

		// Start the test by enqueueing the right event
		if test.addedVA != nil {
			ctrl.vaAdded(test.addedVA)
		}
		if test.updatedVA != nil {
			ctrl.vaUpdated(test.updatedVA, test.updatedVA)
		}
		if test.deletedVA != nil {
			ctrl.vaDeleted(test.deletedVA)
		}
		if test.updatedPV != nil {
			ctrl.pvUpdated(test.updatedPV, test.updatedPV)
		}

		// Process the queue until we get expected results
		timeout := time.Now().Add(10 * time.Second)
		lastReportedActionCount := 0
		for {
			if time.Now().After(timeout) {
				t.Errorf("Test %q: timed out", test.name)
				break
			}
			if ctrl.vaQueue.Len() > 0 {
				glog.V(5).Infof("Test %q: %d events in VA queue, processing one", test.name, ctrl.vaQueue.Len())
				ctrl.syncVA()
			}
			if ctrl.pvQueue.Len() > 0 {
				glog.V(5).Infof("Test %q: %d events in PV queue, processing one", test.name, ctrl.vaQueue.Len())
				ctrl.syncPV()
			}
			if ctrl.vaQueue.Len() > 0 || ctrl.pvQueue.Len() > 0 {
				// There is still some work in the queue, process it now
				continue
			}
			currentActionCount := len(client.Actions())
			if currentActionCount < len(test.expectedActions) {
				if lastReportedActionCount < currentActionCount {
					glog.V(5).Infof("Test %q: got %d actions out of %d, waiting for the rest", test.name, currentActionCount, len(test.expectedActions))
					lastReportedActionCount = currentActionCount
				}
				// The test expected more to happen, wait for them
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}

		actions := client.Actions()
		for i, action := range actions {
			if len(test.expectedActions) < i+1 {
				t.Errorf("Test %q: %d unexpected actions: %+v", test.name, len(actions)-len(test.expectedActions), spew.Sdump(actions[i:]))
				break
			}

			// Sanitize time in attach/detach errors
			if action.GetVerb() == "update" && action.GetResource().Resource == "volumeattachments" {
				obj := action.(core.UpdateAction).GetObject()
				o := obj.(*storage.VolumeAttachment)
				if o.Status.AttachError != nil {
					o.Status.AttachError.Time = metav1.Time{}
				}
				if o.Status.DetachError != nil {
					o.Status.DetachError.Time = metav1.Time{}
				}
			}

			expectedAction := test.expectedActions[i]
			if !reflect.DeepEqual(expectedAction, action) {
				t.Errorf("Test %q: action %d\nExpected:\n%s\ngot:\n%s", test.name, i, spew.Sdump(expectedAction), spew.Sdump(action))
				continue
			}
		}

		if len(test.expectedActions) > len(actions) {
			t.Errorf("Test %q: %d additional expected actions", test.name, len(test.expectedActions)-len(actions))
			for _, a := range test.expectedActions[len(actions):] {
				t.Logf("    %+v", a)
			}
		}

		if test.additionalCheck != nil {
			test.additionalCheck(t, test)
		}
		glog.Infof("Test %q: finished \n\n", test.name)
	}
}

// Helper function to create various objects
const (
	testAttacherName = "csi/test"
	testPVName       = "pv1"
	testNodeName     = "node1"
	testVolumeHandle = "handle1"
	testNodeID       = "nodeID1"
)

func createVolumeAttachment(attacher string, pvName string, nodeName string, attached bool, finalizers string, annotations map[string]string) *storage.VolumeAttachment {
	va := &storage.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvName + "-" + nodeName,
			Annotations: annotations,
		},
		Spec: storage.VolumeAttachmentSpec{
			Attacher: attacher,
			NodeName: nodeName,
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &pvName,
			},
		},
		Status: storage.VolumeAttachmentStatus{
			Attached: attached,
		},
	}
	if len(finalizers) > 0 {
		va.Finalizers = strings.Split(finalizers, ",")
	}
	return va
}

func va(attached bool, finalizers string, annotations map[string]string) *storage.VolumeAttachment {
	return createVolumeAttachment(testAttacherName, testPVName, testNodeName, attached, finalizers, annotations)
}

func deleted(va *storage.VolumeAttachment) *storage.VolumeAttachment {
	va.DeletionTimestamp = &metav1.Time{}
	return va
}

func vaWithMetadata(va *storage.VolumeAttachment, metadata map[string]string) *storage.VolumeAttachment {
	va.Status.AttachmentMetadata = metadata
	return va
}

func vaWithNoPVReference(va *storage.VolumeAttachment) *storage.VolumeAttachment {
	va.Spec.Source.PersistentVolumeName = nil
	return va
}

func vaWithInvalidDriver(va *storage.VolumeAttachment) *storage.VolumeAttachment {
	return createVolumeAttachment("unknownDriver", testPVName, testNodeName, false, "", nil)
}

func vaWithAttachError(va *storage.VolumeAttachment, message string) *storage.VolumeAttachment {
	va.Status.AttachError = &storage.VolumeError{
		Message: message,
		Time:    metav1.Time{},
	}
	return va
}

func vaWithDetachError(va *storage.VolumeAttachment, message string) *storage.VolumeAttachment {
	va.Status.DetachError = &storage.VolumeError{
		Message: message,
		Time:    metav1.Time{},
	}
	return va
}

// Fake CSIConnection implementation that check that Attach/Detach is called
// with the right parameters and it returns proper error code and metadata.
type fakeCSIConnection struct {
	calls []csiCall
	index int
	t     *testing.T
}

func (f *fakeCSIConnection) GetDriverName(ctx context.Context) (string, error) {
	return "", fmt.Errorf("Not implemented")
}

func (f *fakeCSIConnection) SupportsPluginControllerService(ctx context.Context) (bool, error) {
	return false, fmt.Errorf("Not implemented")
}

func (f *fakeCSIConnection) SupportsControllerPublish(ctx context.Context) (bool, error) {
	return false, fmt.Errorf("Not implemented")
}

func (f *fakeCSIConnection) Attach(ctx context.Context, volumeID string, readOnly bool, nodeID string, caps *csi.VolumeCapability, attributes, secrets map[string]string) (map[string]string, bool, error) {
	if f.index >= len(f.calls) {
		f.t.Errorf("Unexpected CSI Attach call: volume=%s, node=%s, index: %d, calls: %+v", volumeID, nodeID, f.index, f.calls)
		return nil, true, fmt.Errorf("unexpected call")
	}

	call := f.calls[f.index]
	f.index++

	// Force a delay
	if call.delay != time.Duration(0) {
		time.Sleep(call.delay)
	}

	var err error
	if call.functionName != "attach" {
		f.t.Errorf("Unexpected CSI Attach call: volume=%s, node=%s, expected: %s", volumeID, nodeID, call.functionName)
		err = fmt.Errorf("unexpected attach call")
	}

	if call.volumeHandle != volumeID {
		f.t.Errorf("Wrong CSI Attach call: volume=%s, node=%s, expected PV: %s", volumeID, nodeID, call.volumeHandle)
		err = fmt.Errorf("unexpected attach call")
	}

	if call.nodeID != nodeID {
		f.t.Errorf("Wrong CSI Attach call: volume=%s, node=%s, expected Node: %s", volumeID, nodeID, call.nodeID)
		err = fmt.Errorf("unexpected attach call")
	}

	if !reflect.DeepEqual(call.volumeAttributes, attributes) {
		f.t.Errorf("Wrong CSI Attach call: volume=%s, node=%s, expected attributes %+v, got %+v", volumeID, nodeID, call.volumeAttributes, attributes)
	}

	if !reflect.DeepEqual(call.secrets, secrets) {
		f.t.Errorf("Wrong CSI Attach call: volume=%s, node=%s, expected secrets %+v, got %+v", volumeID, nodeID, call.secrets, secrets)
	}

	if err != nil {
		return nil, true, err
	}
	return call.metadata, call.detached, call.err
}

func (f *fakeCSIConnection) Detach(ctx context.Context, volumeID string, nodeID string, secrets map[string]string) (bool, error) {
	if f.index >= len(f.calls) {
		f.t.Errorf("Unexpected CSI Detach call: volume=%s, node=%s, index: %d, calls: %+v", volumeID, nodeID, f.index, f.calls)
		return true, fmt.Errorf("unexpected call")
	}
	call := f.calls[f.index]
	f.index++

	// Force a delay
	if call.delay != time.Duration(0) {
		time.Sleep(call.delay)
	}

	var err error
	if call.functionName != "detach" {
		f.t.Errorf("Unexpected CSI Detach call: volume=%s, node=%s, expected: %s", volumeID, nodeID, call.functionName)
		err = fmt.Errorf("unexpected detach call")
	}

	if call.volumeHandle != volumeID {
		f.t.Errorf("Wrong CSI Detach call: volume=%s, node=%s, expected PV: %s", volumeID, nodeID, call.volumeHandle)
		err = fmt.Errorf("unexpected detach call")
	}

	if call.nodeID != nodeID {
		f.t.Errorf("Wrong CSI Detach call: volume=%s, node=%s, expected Node: %s", volumeID, nodeID, call.nodeID)
		err = fmt.Errorf("unexpected detach call")
	}

	if !reflect.DeepEqual(call.secrets, secrets) {
		f.t.Errorf("Wrong CSI Detach call: volume=%s, node=%s, expected secrets %+v, got %+v", volumeID, nodeID, call.secrets, secrets)
	}

	if err != nil {
		return true, err
	}
	return call.detached, call.err
}

func (f *fakeCSIConnection) Close() error {
	return fmt.Errorf("Not implemented")
}

func (f *fakeCSIConnection) Probe(ctx context.Context) error {
	return nil
}
