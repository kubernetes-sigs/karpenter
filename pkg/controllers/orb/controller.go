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
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/awslabs/operatorpkg/singleton"

	"google.golang.org/protobuf/proto"
	// proto "github.com/gogo/protobuf/proto" // This one is outdated and causes errors in serialization process
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
) // mountPath = "/data" // Worth including? As defined in our PVC yaml

type Controller struct {
	schedulingInputHeap    *SchedulingInputHeap    // Batches logs of inputs to heap
	schedulingMetadataHeap *SchedulingMetadataHeap // batches logs of scheduling metadata to heap
	mostRecentBaseline     *SchedulingInput        // The most recently saved baseline scheduling input
	baselineSize           int                     // The size of the currently basedlined SchedulingInput in bytes
	rebaselineThreshold    float32                 // The percentage threshold (between 0 and 1)
	deltaToBaselineAvg     float32                 // The average delta to the baseline, moving average
	shouldRebaseline       bool                    // Whether or not we should rebaseline (when the threshold is crossed)
}

func NewController(schedulingInputHeap *SchedulingInputHeap, schedulingMetadataHeap *SchedulingMetadataHeap) *Controller {
	return &Controller{
		schedulingInputHeap:    schedulingInputHeap,
		schedulingMetadataHeap: schedulingMetadataHeap,
		shouldRebaseline:       true,
		rebaselineThreshold:    initialDeltaThreshold,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// ctx = injection.WithControllerName(ctx, "orb.batcher") // What is this for?

	fmt.Println("----------  Starting an ORB Reconcile Cycle  ----------") // For debugging, delete later.

	// Log the scheduling inputs from the heap into either baseline or differences
	err := c.logSchedulingInputsToPV()
	if err != nil {
		fmt.Println("Error writing scheduling inputs to PV:", err)
		return reconcile.Result{}, err
	}

	err = c.logSchedulingMetadataToPV(c.schedulingMetadataHeap)
	if err != nil {
		fmt.Println("Error writing scheduling metadata to PV:", err)
		return reconcile.Result{}, err
	}

	fmt.Println("----------- Ending an ORB Reconcile Cycle -----------")
	fmt.Println()

	return reconcile.Result{RequeueAfter: time.Second * 30}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("orb.batcher").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) logSchedulingInputsToPV() error {
	for c.schedulingInputHeap.Len() > 0 { // Pop each scheduling input off my heap (oldest first) and batch log in PV
		currentInput := c.schedulingInputHeap.Pop().(SchedulingInput)

		// Set the baseline on initial input or upon rebaselining
		if c.mostRecentBaseline == nil || c.shouldRebaseline {
			err := c.logSchedulingBaselineToPV(&currentInput)
			if err != nil {
				fmt.Println("Error saving baseline to PV:", err)
				return err
			}
			c.shouldRebaseline = false
			c.mostRecentBaseline = &currentInput
		} else { // Check if the scheduling inputs have changed since the last time we saved it to PV
			inputDiffAdded, inputDiffRemoved, inputDiffChanged := currentInput.Diff(c.mostRecentBaseline)
			err := c.logSchedulingDifferencesToPV(inputDiffAdded, inputDiffRemoved, inputDiffChanged, currentInput.Timestamp)
			if err != nil {
				fmt.Println("Error saving differences to PV:", err)
				return err
			}
		}
	}
	return nil
}

func (c *Controller) logSchedulingBaselineToPV(item *SchedulingInput) error {
	logdata, err := MarshalSchedulingInput(item)
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}
	c.baselineSize = len(logdata)

	// Set up file naming schema
	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputBaseline_%s.log", timestampStr)
	path := filepath.Join("/data", fileName)

	// DEBUG Remove Later
	fileNametest := fmt.Sprintf("SchedulingInputBaselineTEST_%s.log", timestampStr)
	pathtest := filepath.Join("/data", fileNametest)
	file, err := os.Create(pathtest)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	_, err = file.WriteString(item.String())
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}

	fmt.Println("Writing baseline data to S3 bucket.") // test print / remove later
	return c.writeToPV(logdata, path)
}

// TODO: Eventually merge these individual difference prints to all the differences within a batch (similar to metadata)
func (c *Controller) logSchedulingDifferencesToPV(DiffAdded *SchedulingInput, DiffRemoved *SchedulingInput,
	DiffChanged *SchedulingInput, timestamp time.Time) error {
	logdata, err := MarshalDifferences(DiffAdded, DiffRemoved, DiffChanged)
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}

	c.shouldRebaseline = c.determineRebaseline(len(logdata))

	timestampStr := timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputDifferences_%s.log", timestampStr)
	path := filepath.Join("/data", fileName)

	fmt.Println("Writing differences data to S3 bucket.") // test print / remove later
	return c.writeToPV(logdata, path)
}

func (c *Controller) logSchedulingMetadataToPV(heap *SchedulingMetadataHeap) error {
	if heap == nil || heap.Len() == 0 {
		return nil // Nothing to log.
	}

	// Set up file name schema for batch of metadata
	oldestStr := (*heap)[0].Timestamp.Format("2006-01-02_15-04-05")
	newestStr := (*heap)[len(*heap)-1].Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingMetadata_%s_to_%s.log", oldestStr, newestStr)
	path := filepath.Join("/data", fileName)

	// Pop each scheduling metadata off its heap (oldest first) to batch log them together to PV.
	mapping := &pb.SchedulingMetadataMap{}
	for heap.Len() > 0 {
		metadata := heap.Pop().(SchedulingMetadata)
		entry := &pb.SchedulingMetadataMap_MappingEntry{
			Action:    metadata.Action,
			Timestamp: metadata.Timestamp.Format("2006-01-02_15-04-05"),
		}
		mapping.Entries = append(mapping.Entries, entry)
	}

	// Marshals the mapping
	mappingdata, err := proto.Marshal(mapping)
	if err != nil {
		fmt.Println("Error marshalling data:", err)
		return err
	}

	fmt.Println("Writing metadata to S3 bucket!")
	return c.writeToPV(mappingdata, path)
}

// Log data to the mounted Persistent Volume
func (c *Controller) writeToPV(logdata []byte, path string) error {
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

// Determines if we should save a new baseline Scheduling Input, using a moving-average heuristic
func (c *Controller) determineRebaseline(diffSize int) bool {
	diffSizeFloat := float32(diffSize)
	baselineSizeFloat := float32(c.baselineSize)

	// If differences' size exceeds threshold percentage, rebaseline and update moving average
	if diffSizeFloat > c.rebaselineThreshold*baselineSizeFloat {
		c.baselineSize = diffSize
		c.deltaToBaselineAvg = float32(diffSize) / baselineSizeFloat
		return true
	}

	// Updates the Threshold Value
	deltaToBaselineRatio := diffSizeFloat / baselineSizeFloat
	c.deltaToBaselineAvg = (c.deltaToBaselineAvg * decayFactor) + (deltaToBaselineRatio * updateFactor)
	c.rebaselineThreshold = float32(math.Max(float64(minThreshold), float64(c.deltaToBaselineAvg*thresholdMultiplier)))
	return false
}
