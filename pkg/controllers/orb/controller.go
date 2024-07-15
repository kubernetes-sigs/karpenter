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
)

type Controller struct {
	SIheap                    *SchedulingInputHeap // Batches logs in a Queue
	mostRecentSchedulingInput SchedulingInput      // The most recently saved filename (for checking for changes)
}

// TODO: add struct elements and their instantiations, when defined
func NewController(SIheap *SchedulingInputHeap) *Controller {
	return &Controller{
		SIheap:                    SIheap,
		mostRecentSchedulingInput: SchedulingInput{},
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
		item := c.SIheap.Pop().(SchedulingInput) // Min heap, so always pops the oldest

		err := c.SaveToPV(item)
		if err != nil {
			fmt.Println("Error saving to PV:", err)
			return reconcile.Result{}, err
		}

		// err = c.testReadPVandReconstruct(item)
		// if err != nil {
		// 	fmt.Println("Error reconstructing from PV:", err)
		// 	return reconcile.Result{}, err
		// }
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

/* This function saves things to our Persistent Volume */
// Saves data to PV (S3 Bucket for AWS) via the mounted log path
// It takes a name of the log file as well as the logline to be logged.
// The function opens a file for writing, writes some data to the file, and then closes the file
func (c *Controller) SaveToPV(item SchedulingInput) error {

	//fmt.Println("Saving Scheduling Input to PV:\n", item.String()) // Test print
	// logdata, err := item.Marshal()
	// if err != nil {
	// 	fmt.Println("Error converting Scheduling Input to Protobuf:", err)
	// 	return err
	// }

	// TODO: Instead of the above, In the interim while I figure out the custom protobuf... Just send string to file
	logdata := item.String()

	// Timestamp the file
	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInput_%s.log", timestampStr)

	path := filepath.Join("/data", fileName) // mountPath = /data by PVC

	// Opens the mounted volume (S3 Bucket) file at that path
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	// // Writes serialized data to the file
	// _, err = file.Write(logdata)
	// if err != nil {
	// 	fmt.Println("Error writing data to file:", err)
	// 	return err
	// }

	// TODO: Uncomment above; Testing Version // only here while testing string print
	_, err = fmt.Fprintln(file, logdata)
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}

	fmt.Println("Data written to S3 bucket successfully!")
	return nil
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

// Function for reconstructing inputs
func ReconstructSchedulingInput(fileName string) error {

	// Read from the PV to check (will be what the ORB tool does from the Command Line)
	readTimestamp, readdata, err := ReadFromPV(fileName)
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

func testReadPVandReconstruct(item SchedulingInput) error {
	// We're sort of artificially rebuilding the filename here, just to do a loopback test of sorts.
	// In reality, we could just pull a file from a known directory
	timestampStr := item.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("ProvisioningSchedulingInput_%s.log", timestampStr)

	err := ReconstructSchedulingInput(fileName)
	if err != nil {
		fmt.Println("Error reconstructing scheduling input:", err)
		return err
	}
	return nil
}
