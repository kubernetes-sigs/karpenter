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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Set global variable(s) for Mounted PV path
var mountPath = "/data"

// const (
// 	orbQueueBaseDelay = 100 * time.Millisecond
// 	orbQueueMaxDelay  = 10 * time.Second
// )

type Queue struct {
	//workqueue.RateLimitingInterface
	//mu  sync.Mutex
	Set sets.Set[string]
}

func NewQueue() *Queue {
	queue := &Queue{
		//RateLimitingInterface: workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(orbQueueBaseDelay, orbQueueMaxDelay)),
		Set: sets.New[string](),
	}
	return queue
}

// This will take the info we pass from Provisioner or Disruption to log
func (q *Queue) LogLine(item string) {
	// Do some data validation?

	// Serialize it into the protobuffed structure binary?

	// Then insert it...?
	q.Set.Insert(item)
}

// This function serializes _ resource into protobuf

// This functions deserialized _ resource into protobuf

type Controller struct {
	queue *Queue
	// some sort of log store, or way to batch logs together before sending to PV
}

// TODO: add struct elements and their instantiations, when defined
func NewController(queue *Queue) *Controller {
	return &Controller{
		queue: queue,
	}
}

// This function batches together loglines into our Queue data structure
// This queue will be periodically dumped to the S3 Bucket
func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// TODO: what does this do / where does it reference to or need to reference to?
	// ctx = injection.WithControllerName(ctx, "orb.batcher")

	// fmt.Println("Starting One Reconcile Print from ORB...")

	// c.queue.Set.Insert("Hello World from the ORB Batcher Reconciler")

	// // for items in the Queue, print them
	// for item := range c.queue.Set {
	// 	fmt.Println(item)
	// }

	// fmt.Println("Ending One Reconcile Print from ORB...")
	// fmt.Println()

	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

// TODO: What does this register function do? Is it needed for a controller to work?
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("orb.batcher").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
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

/* This function saves things to our Persistent Volume */
// Saves data to PV (S3 Bucket for AWS) via the mounted log path
// It takes a name of the log file as well as the logline to be logged.
// The function opens a file for writing, writes some data to the file, and then closes the file
func saveToS3Bucket(logname string, logline string) error {
	// Create the log file path and desired logline (example for now)
	sanitizedname := sanitizePath(logname)
	path := filepath.Join(mountPath, sanitizedname)
	//logline := fmt.Sprintf("Printing data (from %s) to the S3 bucket", logname)

	// Opens the mounted volume (S3 Bucket) file at that path
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	// Writes data to the file
	_, err = fmt.Fprintln(file, logline)
	if err != nil {
		fmt.Println("Error writing to file:", err)
		return err
	}

	fmt.Println("Data written to S3 bucket successfully!")
	return nil
}

/* This function pulls things from our Persistent Volume */
// Function to read (and print out?) the logged data from file
func readFromS3Bucket(logname string) error {
	// Create the log file path
	sanitizedname := sanitizePath(logname)
	path := filepath.Join(mountPath, sanitizedname)

	file, err := os.Open(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	// Read the entire file into a byte slice
	data, err := io.ReadAll(file)
	if err != nil {
		fmt.Println("Error reading file:", err)
		return err
	}

	// Print the contents of the file
	fmt.Println("File Contents:")
	fmt.Println(string(data)) // converts data byteslice to string and prints

	// (If I want to read line by line, use this instead) // Read the file line by line
	// scanner := bufio.NewScanner(file)
	// counter := 0
	// for scanner.Scan() {
	// 	line := scanner.Text()
	// 	fmt.Printf("Line #%d: %s\n", counter, line)
	// }

	// // Check for any errors that occurred during scanning
	// if err := scanner.Err(); err != nil {
	// 	fmt.Println("Error reading file:", err)
	// }
	return nil
}
