/*
Copyright The Kubernetes Authors.

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

package orbbatcher

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"

	//"google.golang.org/protobuf/proto"
	proto "github.com/gogo/protobuf/proto"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// Timestamp, dynamic inputs (like pending pods, statenodes, etc.)
type SchedulingInput struct {
	Timestamp   time.Time
	PendingPods []*v1.Pod
	//all the other scheduling inputs...
}

func (si SchedulingInput) String() string {
	return fmt.Sprintf("Timestamp: %v\nPendingPods:\n%v",
		si.Timestamp.Format("2006-01-02_15-04-05"),
		PodsToString(si.PendingPods))
}

// Function take a Scheduling Input to []byte, marshalled as a protobuf
// TODO: With a custom-defined .proto, this will look different.
func (si SchedulingInput) Marshal() ([]byte, error) {
	podList := &v1.PodList{
		Items: make([]v1.Pod, 0, len(si.PendingPods)),
	}

	for _, podPtr := range si.PendingPods {
		podList.Items = append(podList.Items, *podPtr)
	}
	return podList.Marshal()
}

// Function to do the reverse, take a scheduling input's []byte and unmarshal it back into a SchedulingInput
func PBToSchedulingInput(timestamp time.Time, data []byte) (SchedulingInput, error) {
	podList := &v1.PodList{}
	if err := proto.Unmarshal(data, podList); err != nil {
		return SchedulingInput{}, fmt.Errorf("unmarshaling pod list, %w", err)
	}
	pods := lo.ToSlicePtr(podList.Items)
	return ReconstructedSchedulingInput(timestamp, pods), nil
}

// This defines a min-heap of SchedulingInputs by slice,
// with the Timestamp field defined as the comparator
type SchedulingInputHeap []SchedulingInput //heaps are thread-safe in container/heap

func (h SchedulingInputHeap) Len() int {
	return len(h)
}

func (h SchedulingInputHeap) Less(i, j int) bool {
	return h[i].Timestamp.Before(h[j].Timestamp)
}

func (h SchedulingInputHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *SchedulingInputHeap) Push(x interface{}) {
	*h = append(*h, x.(SchedulingInput))
}

func (h *SchedulingInputHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func NewSchedulingInputHeap() *SchedulingInputHeap {
	h := &SchedulingInputHeap{}
	heap.Init(h)
	return h
}

func NewSchedulingInput(pendingPods []*v1.Pod) SchedulingInput {
	return SchedulingInput{
		Timestamp:   time.Now(),
		PendingPods: pendingPods,
	}
}

// Reconstruct a scheduling input (presumably from a file)
func ReconstructedSchedulingInput(timestamp time.Time, pendingPods []*v1.Pod) SchedulingInput {
	return SchedulingInput{
		Timestamp:   timestamp,
		PendingPods: pendingPods,
	}
}

type Controller struct {
	SIheap             *SchedulingInputHeap // Batches logs in a Queue
	mostRecentFilename string               // The most recently saved filename (for checking for changes)
}

// TODO: add struct elements and their instantiations, when defined
func NewController(SIheap *SchedulingInputHeap) *Controller {
	return &Controller{
		SIheap:             SIheap,
		mostRecentFilename: "", // Initialize with an empty string
		//TODO: this isn't consistent through restarts of Karpenter. Would want a way to pull the most recent. Maybe a metadata file?
		//      That would have to be a delete/replace since PV files are immutable.
	}
}

// This function batches together loglines into our Queue data structure
// This queue will be periodically dumped to the S3 Bucket
func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// ctx = injection.WithControllerName(ctx, "orb.batcher")

	fmt.Println("----------  Starting a Reconcile Print from ORB  ----------")

	// Pop each scheduling input off my heap (oldest first) and batch log in PV (also loopback test it)
	for c.SIheap.Len() > 0 {
		item := heap.Pop(c.SIheap).(SchedulingInput) // Min heap, so always pops the oldest

		err := c.SaveToPV(item)
		if err != nil {
			fmt.Println("Error saving to PV:", err)
			return reconcile.Result{}, err
		}

		err = c.testReadPVandReconstruct(item)
		if err != nil {
			fmt.Println("Error reconstructing from PV:", err)
			return reconcile.Result{}, err
		}
	}

	fmt.Println("----------- Ending a Reconcile Print from ORB -----------")
	fmt.Println()

	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

// TODO: What does this register function do? Is it needed for a controller to work?
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("orb.batcher").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

/* The following functions are testing toString functions that will mirror what the serialization
   deserialization functions will do in protobuf. These are inefficient, but human-readable */

// TODO: This eventually will be "as simple" as reconstructing the data structures from
// the log data and using K8S and/or Karpenter representation to present as JSON or YAML or something

// This function as a human readable test function for serializing desired pod data
// It takes in a v1.Pod and gets the string representations of all the fields we care about.
func PodToString(pod *v1.Pod) string {
	if pod == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Name: %s, Namespace: %s, Phase: %s, NodeName: %s", pod.Name, pod.Namespace, pod.Status.Phase, pod.Spec.NodeName)
}

func PodsToString(pods []*v1.Pod) string {
	if pods == nil {
		return "<nil>"
	}
	var buf bytes.Buffer
	for _, pod := range pods {
		buf.WriteString(PodToString(pod) + "\n")
	}
	return buf.String()
}

// Similar function for stateNode
func StateNodeToString(node *state.StateNode) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Node: %s, NodeClaim: %s", NodeToString(node.Node), NodeClaimToString(node.NodeClaim))
}

// Similar function for human-readable string serialization of a v1.Node
func NodeToString(node *v1.Node) string {
	if node == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Name: %s, Status: %s, NodeName: %s", node.Name, node.Status.Phase, node.Status.NodeInfo.SystemUUID)
}

// Similar function for NodeClaim
func NodeClaimToString(nodeClaim *v1beta1.NodeClaim) string {
	if nodeClaim == nil {
		return "<nil>"
	}
	return fmt.Sprintf("NodeClaimName: %s", nodeClaim.Name)
}

// Similar for instanceTypes (name, requirements, offerings, capacity, overhead
func InstanceTypeToString(instanceType *cloudprovider.InstanceType) string {
	if instanceType == nil {
		return "<nil>"
	}
	// TODO: String print the sub-types, like Offerings, too, all of them
	return fmt.Sprintf("Name: %s, Requirements: %s, Offerings: %s", instanceType.Name,
		RequirementsToString(&instanceType.Requirements), OfferingToString(&instanceType.Offerings[0]))
}

// Similar for IT Requirements
func RequirementsToString(requirements *scheduling.Requirements) string {
	if requirements == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Requirements: %s", requirements)
}

// Similar for IT Offerings (Requirements, Price, Availability)
func OfferingToString(offering *cloudprovider.Offering) string {
	if offering == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Offering Requirements: %s, Price: %f, Available: %t",
		RequirementsToString(&offering.Requirements), offering.Price, offering.Available)
}

// Function for logging everything in the Provisioner Scheduler (i.e. pending pods, statenodes...)
func (q *SchedulingInputHeap) LogProvisioningScheduler(pods []*v1.Pod, stateNodes []*state.StateNode, instanceTypes map[string][]*cloudprovider.InstanceType) {
	si := NewSchedulingInput(pods) // TODO: add all inputs I want to log
	q.Push(si)                     // sends that scheduling input into the data structure to be popped in batch to go to PV as a protobuf
}

/* This function saves things to our Persistent Volume */
// Saves data to PV (S3 Bucket for AWS) via the mounted log path
// It takes a name of the log file as well as the logline to be logged.
// The function opens a file for writing, writes some data to the file, and then closes the file
func (c *Controller) SaveToPV(item SchedulingInput) error {

	fmt.Println("Saving Scheduling Input to PV:\n", item.String()) // Test print
	logdata, err := item.Marshal()
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}

	// Timestamp the file
	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("ProvisioningSchedulingInput_%s.log", timestampStr)

	path := filepath.Join("/data", fileName) // mountPath = /data by PVC

	// Opens the mounted volume (S3 Bucket) file at that path
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	// Writes data to the file
	_, err = fmt.Fprintln(file, timestampStr)
	if err != nil {
		fmt.Println("Error writing timestamp to file:", err)
		return err
	}

	_, err = file.Write(logdata)
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}

	fmt.Println("Data written to S3 bucket successfully!")
	return nil
}

/* These will be part of the command-line printing representation... */

// For testing, pull pending pod and print as string.
func UnmarshalPod(data []byte) (*v1.Pod, error) {
	pod := &v1.Pod{}
	if err := proto.Unmarshal(data, pod); err != nil {
		fmt.Println("Error unmarshaling pod:", err)
		return nil, err
	}
	return pod, nil
}

// Function to unmarshal and print a pod
func PrintPodPB(data []byte) {
	pod, err := UnmarshalPod(data)
	if err != nil {
		fmt.Println("Error deserializing pod:", err)
		return
	}
	fmt.Println("Pod is: ", PodToString(pod))
}

// Security Issue Common Weakness Enumeration (CWE)-22,23 Path Traversal
// They highly recommend sanitizing inputs before accessing that path.
func (c *Controller) sanitizePath(path string) string {
	// Remove any leading or trailing slashes, "../" or "./"...
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = regexp.MustCompile(`\.\.\/`).ReplaceAllString(path, "")
	path = regexp.MustCompile(`\.\/`).ReplaceAllString(path, "")
	path = strings.ReplaceAll(path, "../", "")

	return path
}

// Function to pull from an S3 bucket
func (c *Controller) ReadFromPV(logname string) (time.Time, []byte, error) {
	sanitizedname := c.sanitizePath(logname)
	path := filepath.Join("/data", sanitizedname)

	// Open the file for reading
	file, err := os.Open(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return time.Time{}, nil, err
	}
	defer file.Close()

	// TODO: This will be as simple as an io.ReadAll for all the contents, once I customize an SI .proto

	// Create a new buffered reader
	reader := bufio.NewReader(file)

	// Read the first line as a string
	timestampStr, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Error reading timestamp:", err)
		return time.Time{}, nil, err
	}
	timestampStr = strings.TrimSuffix(timestampStr, "\n")

	// Read the remaining bytes
	// TODO: This will be bytes at a time until a newline, which will follow a schema
	// defined for Scheduling Inputs in order to best keep track of protobufs and reconstruct
	contents, err := io.ReadAll(reader)
	if err != nil {
		fmt.Println("Error reading file bytes:", err)
		return time.Time{}, nil, err
	}

	timestamp, err := time.Parse("2006-01-02_15-04-05", timestampStr)
	if err != nil {
		fmt.Println("Error parsing timestamp:", err)
		return time.Time{}, nil, err
	}

	return timestamp, contents, nil
}

// Function for reconstructing inputs
func (c *Controller) ReconstructSchedulingInput(fileName string) error {

	// Read from the PV to check (will be what the ORB tool does from the Command Line)
	readTimestamp, readdata, err := c.ReadFromPV(fileName)
	if err != nil {
		fmt.Println("Error reading from PV:", err)
		return err
	}

	// Protobuff to si
	si, err := PBToSchedulingInput(readTimestamp, readdata)
	if err != nil {
		fmt.Println("Error converting PB to SI:", err)
		return err
	}
	// Print the reconstructed scheduling input
	fmt.Println("Reconstructed Scheduling Input looks like:\n" + si.String())
	return nil
}

func (c *Controller) testReadPVandReconstruct(item SchedulingInput) error {
	// We're sort of artificially rebuilding the filename here, just to do a loopback test of sorts.
	// In reality, we could just pull a file from a known directory
	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("ProvisioningSchedulingInput_%s.log", timestampStr)

	err := c.ReconstructSchedulingInput(fileName)
	if err != nil {
		fmt.Println("Error reconstructing scheduling input:", err)
		return err
	}
	return nil
}
