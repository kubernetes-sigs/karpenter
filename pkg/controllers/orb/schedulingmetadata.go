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
	"os"
	"path/filepath"
	"strings"
	"time"
	//"google.golang.org/protobuf/proto"
)

type SchedulingMetadata struct {
	Action    string
	Timestamp time.Time
}

type key int

const (
	schedulingMetadataKey key = iota
)

// case "normal-provisioning":
// case "consolidation-simulation":
// WithSchedulingMetadata returns a new context with the provided provisioning metadata.
func WithSchedulingMetadata(ctx context.Context, action string, timestamp time.Time) context.Context {
	metadata := SchedulingMetadata{
		Action:    action,
		Timestamp: timestamp,
	}
	return context.WithValue(ctx, schedulingMetadataKey, metadata)
}

// GetProvisioningMetadata retrieves the scheduling metadata from the context.
func GetSchedulingMetadata(ctx context.Context) (SchedulingMetadata, bool) {
	metadata, ok := ctx.Value(schedulingMetadataKey).(SchedulingMetadata)
	return metadata, ok
}

// This function will log scheduling action to PV
func LogSchedulingAction(ctx context.Context, timestamp time.Time) error {
	metadata, ok := GetSchedulingMetadata(ctx)
	if !ok { // Provisioning metadata is not set, set it to the default - normal provisioning action
		ctx = WithSchedulingMetadata(ctx, "normal-provisioning", timestamp)
		metadata, _ = GetSchedulingMetadata(ctx) // Get it again to update metadata
	}

	switch metadata.Action { // Regardless of which action, so long as valid, write it.
	case "normal-provisioning", "single-node-consolidation", "multi-node-consolidation":
		fmt.Println("Writing scheduling metadata to PV:", metadata.Action) //Testing, remove later
		err := WriteSchedulingMetadataToPV(metadata)
		if err != nil {
			return err
		}
	default:
		fmt.Println("Invalid scheduling action metadata:", metadata.Action) //Testing, remove later
		return fmt.Errorf("invalid scheduling action metadata: %s", metadata.Action)
	}
	return nil

	// TODO: Once we log it, we need to clear it from the context.
}

func WriteSchedulingMetadataToPV(metadata SchedulingMetadata) error {
	timestampStr := metadata.Timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingActionMetadata_%s.log", timestampStr)
	path := filepath.Join("/data", fileName)

	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	_, err = fmt.Fprintln(file, timestampStr+"\t"+metadata.Action)
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}

	fmt.Println("Metadata written to S3 bucket successfully!")
	return nil
}

// Reads in and parses all the scheduling metadata from the file in the PV.
func ReadSchedulingMetadataFromPV(timestampStr string) ([]*SchedulingMetadata, error) {
	fileName := fmt.Sprintf("SchedulingActionMetadata_%s.log", timestampStr)
	path := filepath.Join("/data", fileName)

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var existingmetadata []*SchedulingMetadata
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid line format: %s", line)
		}

		timestamp, err := time.Parse("2006-01-02_15-04-05", parts[1])
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp: %v", err)
		}

		existingmetadata = append(existingmetadata, &SchedulingMetadata{
			Timestamp: timestamp,
			Action:    parts[0],
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return existingmetadata, nil
}
