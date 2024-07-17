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

package orb

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/singleton"

	//"google.golang.org/protobuf/proto"
	proto "github.com/gogo/protobuf/proto"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
)

const ( // Constants for calculating the moving average of the rebaseline
	initialDeltaThreshold = 0.50
	decayFactor           = 0.9
	updateFactor          = 0.1
	thresholdMultiplier   = 1.2
	minThreshold          = 0.1
)

type Controller struct {
	schedulingInputHeap    *SchedulingInputHeap    // Batches logs of inputs to heap
	schedulingMetadataHeap *SchedulingMetadataHeap // batches logs of scheduling metadata to heap
	mostRecentBaseline     *SchedulingInput        // The most recently saved baseline scheduling input
	baselineSize           int                     // The size of the currently basedlined SchedulingInput in bytes
	rebaselineThreshold    float32                 // The percentage threshold (between 0 and 1)
	deltaToBaselineAvg     float32                 // The average delta to the baseline, moving average
	rebaseline             bool                    // Whether or not we should rebaseline (when the threshold is crossed)
}

// TODO: add struct elements and their instantiations, when defined
func NewController(schedulingInputHeap *SchedulingInputHeap, schedulingMetadataHeap *SchedulingMetadataHeap) *Controller {
	return &Controller{
		schedulingInputHeap:    schedulingInputHeap,
		schedulingMetadataHeap: schedulingMetadataHeap,
		mostRecentBaseline:     nil,
		rebaseline:             true,
		rebaselineThreshold:    initialDeltaThreshold,
		//TODO: this isn't consistent through restarts of Karpenter. Would want a way to pull the most recent. Maybe a metadata file?
		//      That would have to be a delete/replace since PV files are immutable.
	}
}

// This function batches together loglines into our Queue data structure
// This queue will be periodically dumped to the S3 Bucket
func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// ctx = injection.WithControllerName(ctx, "orb.batcher")

	fmt.Println("----------  Starting an ORB Reconcile Cycle  ----------")

	// Pop each scheduling input off my heap (oldest first) and batch log in PV
	for c.schedulingInputHeap.Len() > 0 {
		currentInput := c.schedulingInputHeap.Pop().(SchedulingInput)
		inputDiffAdded, inputDiffRemoved, inputDiffChanged := &SchedulingInput{}, &SchedulingInput{}, &SchedulingInput{}

		// Set the baseline on initial input or upon rebaselining
		if c.mostRecentBaseline == nil || c.rebaseline {
			err := c.logSchedulingBaselineToPV(currentInput)
			if err != nil {
				fmt.Println("Error saving to PV:", err)
				return reconcile.Result{}, err
			}
			c.rebaseline = false
			c.mostRecentBaseline = &currentInput
		} else { // Check if the scheduling inputs have changed since the last time we saved it to PV
			inputDiffAdded, inputDiffRemoved, inputDiffChanged = currentInput.Diff(c.mostRecentBaseline)
			err := c.logSchedulingDifferencesToPV(*inputDiffAdded, *inputDiffRemoved, *inputDiffChanged, currentInput.Timestamp)
			if err != nil {
				fmt.Println("Error saving to PV:", err)
				return reconcile.Result{}, err
			}
		}

		// Updates the internal differences every loop, as opposed to every baseline.
		// This requires reconstruction at an arbitrary time to take in the last baseline and the whole list of changes since then.
		// A more memory intensive way would be to only internally keep the last baseline printed, than any diff+baseline would be reconstructable.
		// c.mostRecentSchedulingInput = &currentInput

		// // (also loopback test it)
		// err := c.testReadPVandReconstruct(item)
		// if err != nil {
		// 	fmt.Println("Error reconstructing from PV:", err)
		// 	return reconcile.Result{}, err
		// }
	}

	// Pop each scheduling metadata off its heap (oldest first) and batch log to PV.
	if c.schedulingMetadataHeap.Len() > 0 {
		err := c.logSchedulingMetadataToPV(c.schedulingMetadataHeap)
		if err != nil {
			fmt.Println("Error writing scheduling metadata to PV:", err)
			return reconcile.Result{}, err
		}
	}

	fmt.Println("----------- Ending an ORB Reconcile Cycle -----------")
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

// Wrapper function for saving scheduling input to PV
func (c *Controller) logSchedulingBaselineToPV(item SchedulingInput) error {
	logdata, err := item.Marshal()
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}

	c.baselineSize = len(logdata)

	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputBaseline_%s.log", timestampStr)
	path := filepath.Join("/data", fileName) // mountPath := /data in our PVC yaml

	fmt.Println("Writing baseline data to S3 bucket.") // test print / remove later
	return c.writeToPV(logdata, path)
}

// Wrapper function for saving scheduling input to PV
// TODO: Eventually merge these individual difference prints to all the differences within a batch (similar to metadata)
func (c *Controller) logSchedulingDifferencesToPV(DiffAdded SchedulingInput, DiffRemoved SchedulingInput,
	DiffChanged SchedulingInput, timestamp time.Time) error {
	logdata, err := MarshalDifferences(protoDifferences(DiffAdded, DiffRemoved, DiffChanged))
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}

	// Trigger a Rebaseline if necessary
	c.rebaseline = c.updateRebaseline(len(logdata))

	timestampStr := timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputDifferences_%s.log", timestampStr)
	path := filepath.Join("/data", fileName) // mountPath := /data in our PVC yaml

	fmt.Println("Writing differences data to S3 bucket.") // test print / remove later
	return c.writeToPV(logdata, path)
}

func (c *Controller) logSchedulingMetadataToPV(heap *SchedulingMetadataHeap) error {
	if heap == nil || heap.Len() == 0 {
		return fmt.Errorf("called with invalid heap or empty heap")
	}

	oldestStr := (*heap)[0].Timestamp.Format("2006-01-02_15-04-05")
	newestStr := (*heap)[len(*heap)-1].Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingMetadata_%s_to_%s.log", oldestStr, newestStr)
	path := filepath.Join("/data", fileName)

	// Pop each scheduling metadata off its heap (oldest first) and batch log to PV.
	mapping := &pb.SchedulingMetadataMapping{}
	for heap.Len() > 0 {
		metadata := heap.Pop().(SchedulingMetadata)
		entry := &pb.SchedulingMetadataMapping_MappingEntry{
			Action:    metadata.Action,
			Timestamp: metadata.Timestamp.Format("2006-01-02_15-04-05"),
		}
		mapping.Entries = append(mapping.Entries, entry)
	}

	mappingdata, err := proto.Marshal(mapping)
	if err != nil {
		fmt.Println("Error marshalling data:", err)
		return err
	}

	fmt.Println("Writing metadata to S3 bucket!")
	return c.writeToPV(mappingdata, path)
}

// This function saves things to our Persistent Volume, saving data to PV (S3 Bucket for AWS) via the mounted log path
func (c *Controller) writeToPV(logdata []byte, path string) error {
	// Opens the mounted volume (S3 Bucket) file at that path
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	_, err = file.Write(logdata)
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}
	return nil
}

// Functions for a moving average heuristic to decide to rebaseline the files
func (c *Controller) updateRebaseline(diffSize int) bool {
	diffSizeFloat := float32(diffSize)
	baselineSizeFloat := float32(c.baselineSize)

	if diffSizeFloat > c.rebaselineThreshold*baselineSizeFloat {
		c.reBaseline(diffSize)
		return true
	}

	c.updateThreshold(diffSizeFloat, baselineSizeFloat)
	return false
}

func (c *Controller) reBaseline(diffSize int) {
	oldBaselineSizeFloat := float32(c.baselineSize)
	c.baselineSize = diffSize // Update baseline
	c.deltaToBaselineAvg = float32(diffSize) / oldBaselineSizeFloat
}

func (c *Controller) updateThreshold(diffSize, baselineSize float32) {
	deltaToBaselineRatio := diffSize / baselineSize
	c.deltaToBaselineAvg = (c.deltaToBaselineAvg * decayFactor) + (deltaToBaselineRatio * updateFactor)
	c.rebaselineThreshold = float32(math.Max(float64(minThreshold), float64(c.deltaToBaselineAvg*thresholdMultiplier)))
}

// This function tests whether we can read from the PV and reconstruct the data

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
func sanitizePath(path string) string {
	// Remove any leading or trailing slashes, "../" or "./"...
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = regexp.MustCompile(`\.\.\/`).ReplaceAllString(path, "")
	path = regexp.MustCompile(`\.\/`).ReplaceAllString(path, "")
	path = strings.ReplaceAll(path, "../", "")

	return path
}

// Function to pull from an S3 bucket
func ReadFromPV(logname string) (time.Time, []byte, error) {
	sanitizedname := sanitizePath(logname)
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

// // Function for reconstructing inputs
// func ReconstructSchedulingInput(fileName string) error {

// 	// Read from the PV to check (will be what the ORB tool does from the Command Line)
// 	readTimestamp, readdata, err := ReadFromPV(fileName)
// 	if err != nil {
// 		fmt.Println("Error reading from PV:", err)
// 		return err
// 	}

// 	// Protobuff to si
// 	si, err := PBToSchedulingInput(readTimestamp, readdata)
// 	if err != nil {
// 		fmt.Println("Error converting PB to SI:", err)
// 		return err
// 	}
// 	// Print the reconstructed scheduling input
// 	fmt.Println("Reconstructed Scheduling Input looks like:\n" + si.String())
// 	return nil
// }

// func testReadPVandReconstruct(item SchedulingInput) error {
// 	// We're sort of artificially rebuilding the filename here, just to do a loopback test of sorts.
// 	// In reality, we could just pull a file from a known directory
// 	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
// 	fileName := fmt.Sprintf("ProvisioningSchedulingInput_%s.log", timestampStr)

// 	err := ReconstructSchedulingInput(fileName)
// 	if err != nil {
// 		fmt.Println("Error reconstructing scheduling input:", err)
// 		return err
// 	}
// 	return nil
// }
