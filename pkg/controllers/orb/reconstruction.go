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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This function tests whether we can read from the PV and reconstruct the data

/* These will be part of the command-line printing representation... */

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
func ReadFromPV(logname string) ([]byte, error) {
	path := filepath.Join("/data", sanitizePath(logname))
	file, err := os.Open(path)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return nil, err
	}
	defer file.Close()

	contents, err := io.ReadAll(file)
	if err != nil {
		fmt.Println("Error reading file bytes:", err)
		return nil, err
	}
	return contents, nil
}

// Function for reconstructing inputs
// Read from the PV to check (will be what the ORB tool does from the Command Line)
func ReconstructSchedulingInput(fileName string) (*SchedulingInput, error) {
	readdata, err := ReadFromPV(fileName)
	if err != nil {
		fmt.Println("Error reading from PV:", err)
		return nil, err
	}

	si, err := UnmarshalSchedulingInput(readdata)
	if err != nil {
		fmt.Println("Error converting PB to SI:", err)
		return nil, err
	}

	return si, nil
}

// We're sort of artificially rebuilding the filename here, just to do a loopback test of sorts.
// In reality, we could just pull a file from a known directory, for known filename schemas in certain time ranges
func ReadPVandReconstruct(timestamp time.Time) error {
	timestampStr := timestamp.Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("SchedulingInputBaseline_%s.log", timestampStr)

	si, err := ReconstructSchedulingInput(fileName)
	if err != nil {
		fmt.Println("Error reconstructing scheduling input:", err)
		return err
	}

	fmt.Println("Reconstructed Scheduling Input looks like:\n" + si.String())
	reconstructedFilename := fmt.Sprintf("ReconstructedSchedulingInput_%s.log", timestampStr)
	path := filepath.Join("/data", reconstructedFilename)
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return err
	}
	defer file.Close()

	_, err = file.WriteString(si.String())
	if err != nil {
		fmt.Println("Error writing reconstruction to file:", err)
		return err
	}

	fmt.Println("Reconstruction written to file successfully!")
	return nil
}

func DebugWriteSchedulingInputStringToLogFile(item *SchedulingInput, timestampStr string) error {
	fileNameStringtest := fmt.Sprintf("SchedulingInputBaselineTEST_%s.log", timestampStr)
	pathStringtest := filepath.Join("/data", fileNameStringtest)
	file, err := os.Create(pathStringtest)
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

	return nil
}

func DebugWriteSchedulingInputToJSONFile(item *SchedulingInput, timestampStr string) error {
	fileNameJSONtest := fmt.Sprintf("SchedulingInputBaselineTEST_%s.json", timestampStr)
	pathJSONtest := filepath.Join("/data", fileNameJSONtest)
	file, err := os.Create(pathJSONtest)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return err
	}
	defer file.Close()

	jsondata, err := json.Marshal(item)
	if err != nil {
		fmt.Println("Error marshalling data to JSON:", err)
		return err
	}
	_, err = file.Write(jsondata)
	if err != nil {
		fmt.Println("Error writing data to file:", err)
		return err
	}

	return nil
}
