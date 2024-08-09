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
	"container/heap"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	// Warning: This github version of protobuf may get autoimported from go.mod/go.sum definitions.
	// It is outdated and will cause errors in the (de/)serialization processes
	//     proto "github.com/gogo/protobuf/proto"
	"google.golang.org/protobuf/proto"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	pvMountPath = "/data"

	// Constants for rebaselining logic, calculating the moving average of differences' sizes compared to baseline size
	initialDeltaThreshold = 0.50
	decayFactor           = 0.9
	updateFactor          = 1 - decayFactor
	thresholdMultiplier   = 1.2
	minThreshold          = 0.1
)

type Controller struct {
	schedulingInputHeap    *SchedulingInputHeap    // This heap batches logs of inputs every reconcile loop.
	schedulingMetadataHeap *SchedulingMetadataHeap // This heap batches logs of scheduling metadata every reconcile loop.
	mostRecentBaseline     *SchedulingInput        // The most recently saved full scheduling input on which subsequent diffs are based.
	baselineSize           int                     // The size of the currently basedlined SchedulingInput in bytes
	rebaselineThreshold    float32                 // The percentage threshold (between 0 and 1)
	deltaToBaselineAvg     float32                 // The average delta to the baseline, moving average
	shouldRebaseline       bool                    // Whether or not we should rebaseline (when the threshold is crossed)
}

func NewController(schedulingInputHeap *SchedulingInputHeap, schedulingMetadataHeap *SchedulingMetadataHeap) *Controller {
	return &Controller{
		schedulingInputHeap:    schedulingInputHeap,
		schedulingMetadataHeap: schedulingMetadataHeap,
		mostRecentBaseline:     nil,
		shouldRebaseline:       true, // Always rebaseline at start-up / on first reconcile
		rebaselineThreshold:    initialDeltaThreshold,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "orb.batcher") //nolint:ineffassign,staticcheck

	err := c.logSchedulingInputsToPV()
	if err != nil {
		fmt.Println("Error writing scheduling inputs to PV:", err)
		return reconcile.Result{}, err
	}

	err = c.logSchedulingMetadataToPV()
	if err != nil {
		fmt.Println("Error writing scheduling metadata to PV:", err)
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: time.Second * 30}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("orb.batcher").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// Logs the scheduling inputs from the heap as either a baseline or differences
func (c *Controller) logSchedulingInputsToPV() error {
	batchedDifferences := []*SchedulingInputDifferences{}
	for c.schedulingInputHeap.Len() > 0 {
		currentInput := heap.Pop(c.schedulingInputHeap).(SchedulingInput)

		// Set the baseline on initial input or upon rebaselining
		if c.mostRecentBaseline == nil || c.shouldRebaseline {
			err := c.logSchedulingBaselineToPV(&currentInput)
			if err != nil {
				fmt.Println("Error saving baseline to PV:", err)
				return err
			}
			c.mostRecentBaseline = &currentInput
			c.shouldRebaseline = false
		} else { // Batch the scheduling inputs differences since the last time we saved it to PV
			currentDifferences := c.mostRecentBaseline.Diff(&currentInput)
			batchedDifferences = append(batchedDifferences, currentDifferences)
			c.determineRebaseline(currentDifferences.getByteSize())
		}
	}

	err := c.logBatchedSchedulingDifferencesToPV(batchedDifferences)
	if err != nil {
		fmt.Println("Error saving differences to PV:", err)
		return err
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

	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputBaseline_%s.log", timestampStr)
	path := filepath.Join(pvMountPath, fileName)

	return c.writeToPV(logdata, path)
}

func (c *Controller) logBatchedSchedulingDifferencesToPV(batchedDifferences []*SchedulingInputDifferences) error {
	if len(batchedDifferences) == 0 {
		return nil // Nothing to log.
	}

	start, end := GetTimeWindow(batchedDifferences)
	fileName := fmt.Sprintf("SchedulingInputDifferences_%s_%s.log", start.Format("2006-01-02_15-04-05"), end.Format("2006-01-02_15-04-05"))
	path := filepath.Join(pvMountPath, fileName)

	logdata, err := MarshalBatchedDifferences(batchedDifferences)
	if err != nil {
		fmt.Println("Error converting Scheduling Input to Protobuf:", err)
		return err
	}
	return c.writeToPV(logdata, path)
}

// Log the associated scheduling action metadata
func (c *Controller) logSchedulingMetadataToPV() error {
	heap := c.schedulingMetadataHeap
	if heap == nil || heap.Len() == 0 {
		return nil // Nothing to log.
	}

	// Set up file name schema for batch of metadata
	oldestStr := (*heap)[0].Timestamp.Format("2006-01-02_15-04-05")
	newestStr := (*heap)[len(*heap)-1].Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingMetadata_%s_to_%s.log", oldestStr, newestStr)
	path := filepath.Join(pvMountPath, fileName)

	// Marshals the mapping
	mappingdata, err := proto.Marshal(protoSchedulingMetadataMap(heap))
	if err != nil {
		fmt.Println("Error marshaling data:", err)
		return err
	}
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
// The largest portion of the SchedulingInputs are InstanceTypes, so the expectation is that a
// rebaseline will only be triggered when InstanceType offerings change.
func (c *Controller) determineRebaseline(diffSize int) {
	diffSizeFloat := float32(diffSize)
	baselineSizeFloat := float32(c.baselineSize)

	// If differences' size exceeds threshold percentage, rebaseline and update moving average
	if diffSizeFloat > c.rebaselineThreshold*baselineSizeFloat {
		c.baselineSize = diffSize
		c.deltaToBaselineAvg = diffSizeFloat / baselineSizeFloat
		c.shouldRebaseline = true
	} else { // Otherwise, update the threshold
		deltaToBaselineRatio := diffSizeFloat / baselineSizeFloat
		c.deltaToBaselineAvg = (c.deltaToBaselineAvg * decayFactor) + (deltaToBaselineRatio * updateFactor)
		c.rebaselineThreshold = float32(math.Max(float64(minThreshold), float64(c.deltaToBaselineAvg*thresholdMultiplier)))
		c.shouldRebaseline = false
	}
}
