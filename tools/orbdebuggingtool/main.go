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

package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	v1api "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	_ "knative.dev/pkg/system/testing"
	"sigs.k8s.io/karpenter/tools/orbdebuggingtool/pkg"

	"sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/orb"
	pb "sigs.k8s.io/karpenter/pkg/controllers/orb/proto"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/yaml"
)

var (
	logPath               string // Points to where the logs are stored (whether from the user's PV or some local save of the files)
	nodepoolsYamlFilepath string
	nodePools             []*v1.NodePool
	reconstruct           bool
	resimulate            bool
	all                   bool
	reconstructionOutput  string
	outputDir             string
)

// Options of all the scheduling actions for the user to choose off the command-line.
// The scheduling inputs from the associated action is reconstructed based on the timestamp reference.
type SchedulingMetadataOption struct {
	ID        int
	Action    string
	Timestamp time.Time
}

func (o *SchedulingMetadataOption) String() string {
	return fmt.Sprintf("%d. %s (%s)", o.ID, o.Action, o.Timestamp.Format("2006-01-02_15-04-05"))
}

// Alias to define JSON marshaling for YAML output
type PodErrors map[*corev1.Pod]error

func (pe *PodErrors) String() string {
	var sb strings.Builder
	for pod, err := range *pe {
		sb.WriteString(fmt.Sprintf("%s: %s\n", pod.GetName(), err.Error()))
	}
	return sb.String()
}

// Parse the command line arguments
func init() {
	flag.StringVar(&logPath, "dir", "", "Path to the directory containing logs")
	flag.StringVar(&nodepoolsYamlFilepath, "nodepools", "", "Path to the YAML file containing NodePool definitions")
	flag.BoolVar(&reconstruct, "reconstruct", false, "Reconstruct scheduling input(s) and print reconstruction to file. This is much slower than just resimulation.")
	flag.BoolVar(&resimulate, "resimulate", true, "Reconstruct and resimulate scheduling input(s).\nSetting reconstruct to false but resimulate to true will reconstruct for resimulation but not print out the reconstruction.")
	flag.BoolVar(&all, "all", false, "Reconstruct and/or resimulate *all* scheduling inputs for samples provided")
	flag.StringVar(&reconstructionOutput, "rec-output", "yaml", "Output format for reconstructed scheduling input (yaml, json, or both)")
	flag.StringVar(&outputDir, "out", ".", "Output directory for reconstructed scheduling input files")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Validate the reconstructionOutput flag value
	switch strings.ToLower(reconstructionOutput) {
	case "yaml", "json", "both":
		// Valid values, do nothing
	default:
		fmt.Println("Invalid value for -rec-output flag. Valid options: 'yaml', 'json', 'both'. Using default value 'yaml'.")
	}

	// Warning: Without cloudprovider-specific labels, the Difference logic in Requirements/Offerings will fail.
	v1.RestrictedLabelDomains = v1.RestrictedLabelDomains.Insert(v1api.RestrictedLabelDomains...)
	v1.WellKnownLabels = v1.WellKnownLabels.Insert(
		v1api.LabelInstanceHypervisor,
		v1api.LabelInstanceEncryptionInTransitSupported,
		v1api.LabelInstanceCategory,
		v1api.LabelInstanceFamily,
		v1api.LabelInstanceGeneration,
		v1api.LabelInstanceSize,
		v1api.LabelInstanceLocalNVME,
		v1api.LabelInstanceCPU,
		v1api.LabelInstanceCPUManufacturer,
		v1api.LabelInstanceMemory,
		v1api.LabelInstanceEBSBandwidth,
		v1api.LabelInstanceNetworkBandwidth,
		v1api.LabelInstanceGPUName,
		v1api.LabelInstanceGPUManufacturer,
		v1api.LabelInstanceGPUCount,
		v1api.LabelInstanceGPUMemory,
		v1api.LabelInstanceAcceleratorName,
		v1api.LabelInstanceAcceleratorManufacturer,
		v1api.LabelInstanceAcceleratorCount,
		v1api.LabelTopologyZoneID,
		corev1.LabelWindowsBuild,
	)
}

// This conducts ORB Reconstruction and Resimulation from the command-line.
func main() {
	if !reconstruct && !resimulate {
		fmt.Println("Neither reconstruct nor resimulate action specified. Use -reconstruct or -resimulate to perform actions.")
		flag.Usage()
		os.Exit(1)
	}

	options, err := getMetadataOptionsFromLogs()
	if err != nil {
		fmt.Println("Error reading metadata logs:", err)
		return
	}
	if len(options) == 0 {
		fmt.Printf("No scheduling metadata logs found.\nPlease ensure the sample_logs directory at %s has the desired logs.\n", logPath)
		return
	}

	if resimulate {
		nodePools, err = unmarshalNodePoolsFromUser(nodepoolsYamlFilepath)
		if err != nil {
			fmt.Println("Error unmarshalling node pools:", err)
			return
		}
	}

	if all {
		if !promptUserForConfirmation(len(options)) {
			return
		}

		for _, option := range options {
			err = reconstructAndResimulate(option.Timestamp)
			if err != nil {
				fmt.Printf("Error reconstructing/resimulating scheduling input for %s: %v\n", option.Timestamp, err)
				continue
			}
		}
	} else {
		selectedOption := promptUserForOption(options)
		fmt.Printf("\nSelected option: '%s'\n", selectedOption)

		err = reconstructAndResimulate(selectedOption.Timestamp)
		if err != nil {
			fmt.Printf("Error reconstructing/resimulating scheduling input for %s: %v\n", selectedOption.Timestamp, err)
			return
		}
	}
}

// Read all metadata log files in the directory and extract available options of scheduling actions
func getMetadataOptionsFromLogs() ([]*SchedulingMetadataOption, error) {
	files, err := os.ReadDir(logPath)
	if err != nil {
		fmt.Println("Error reading directory:", err)
		return nil, err
	}

	// For each file starting with "SchedulingMetadata", read its contents, deserialize from protobuf and
	// save into it's orb.Metadata structure. Then extracts and sorts each file's metadata and returns their options
	regex := regexp.MustCompile(`^SchedulingMetadata.*\.log$`)
	options := []*SchedulingMetadataOption{}
	allMetadata := orb.NewMinHeap[orb.SchedulingMetadata]()
	for _, file := range files {
		if !regex.MatchString(file.Name()) {
			continue
		}

		contents, err := ReadLog(file.Name())
		if err != nil {
			fmt.Println("Error reading file contents:", err)
			return nil, err
		}

		// Unmarshal the metadataLogdata back into []metadata
		protoMetadataMap := &pb.SchedulingMetadataMap{}
		proto.Unmarshal(contents, protoMetadataMap)
		metadataSlice := orb.ReconstructAllSchedulingMetadata(protoMetadataMap)

		for _, metadatum := range metadataSlice {
			if metadatum != nil {
				heap.Push(allMetadata, *metadatum)
			}
		}
	}

	for allMetadata.Len() > 0 {
		metadata := heap.Pop(allMetadata).(orb.SchedulingMetadata)
		options = append(options, &SchedulingMetadataOption{ID: len(options), Action: metadata.Action, Timestamp: metadata.Timestamp})
	}

	return options, nil
}

func promptUserForConfirmation(numOptions int) bool {
	reader := bufio.NewReader(os.Stdin)

	var action string
	if reconstruct && resimulate {
		action = "reconstruct and resimulate"
	} else if reconstruct {
		action = "reconstruct"
	} else if resimulate {
		action = "resimulate"
	}

	var plurality string
	if numOptions > 1 {
		plurality = "s"
	}

	fmt.Printf("Are you sure you want to %s %d scheduling input%s? (Y/N): ", action, numOptions, plurality)
	response, _ := reader.ReadString('\n')
	response = strings.ToLower(strings.TrimSpace(response))

	if response == "y" || response == "yes" {
		return true
	} else if response == "n" || response == "no" {
		return false
	} else {
		fmt.Println("Invalid response. Please enter 'Yes (Y)' or 'No (N)'.")
		return promptUserForConfirmation(numOptions)
	}
}

func promptUserForOption(options []*SchedulingMetadataOption) *SchedulingMetadataOption {
	fmt.Println("Available options:")
	for _, option := range options {
		fmt.Println(option.String())
	}

	fmt.Print("Enter the option number: ")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Println("Error reading input:", err)
		return promptUserForOption(options)
	}
	choice, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || choice < 0 || choice >= len(options) {
		fmt.Printf("Invalid input \"%s\". Please enter a number between 0 and %d.\n", input, len(options)-1)
		return promptUserForOption(options)
	}
	return options[choice]
}

func reconstructAndResimulate(timestamp time.Time) error {
	reconstructedSchedulingInput, err := Reconstruct(timestamp)
	if err != nil {
		fmt.Printf("Error reconstructing scheduling input for %s: %v\n", timestamp, err)
		return err
	}
	err = Resimulate(reconstructedSchedulingInput, timestamp)
	if err != nil {
		fmt.Printf("Error resimulating for %s: %v\n", timestamp, err)
		return err
	}
	return nil
}

func Reconstruct(reconstructTime time.Time) (*orb.SchedulingInput, error) {
	baselineFilename, differencesFilenames := GetReconstructionFiles(reconstructTime)
	fmt.Println("Finding baseline file: ", baselineFilename)
	reconstructedBaseline, err := ReconstructSchedulingInput(baselineFilename)
	if err != nil {
		fmt.Println("Error executing option:", err)
		return nil, err
	}

	reconstructedDifferences, err := ReconstructDifferences(differencesFilenames)
	if err != nil {
		fmt.Println("Error reconstructing scheduling input differences", err)
		return nil, err
	}

	reconstructedSchedulingInput := orb.MergeDifferences(reconstructedBaseline, reconstructedDifferences, reconstructTime)

	// In either case of reconstruct or resimulate, we need the reconstructed inputs created and returned. The flag is for printing them out.
	if reconstruct {
		switch reconstructionOutput {
		case "yaml":
			writeReconstruction(reconstructedSchedulingInput, "yaml")
		case "json":
			writeReconstruction(reconstructedSchedulingInput, "json")
		case "both":
			writeReconstruction(reconstructedSchedulingInput, "yaml")
			writeReconstruction(reconstructedSchedulingInput, "json")
		} // No default, pre-validated in init()
	}

	return reconstructedSchedulingInput, nil
}

func Resimulate(reconstructedSchedulingInput *orb.SchedulingInput, timestamp time.Time) error {
	if resimulate {
		results, err := pkg.Resimulate(reconstructedSchedulingInput, nodePools)
		if err != nil {
			fmt.Println("Error resimulating:", err)
			return err
		}

		printResults(results, timestamp, nodePools)
	}
	return nil
}

// Function to pull from a directory (either a PV or a local log folder)
func ReadLog(logname string) ([]byte, error) {
	path := filepath.Join(logPath, sanitizePath(logname))
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

// This function tests whether we can read from the PV and reconstruct the data

// Function to get the files which will reconstruct a scheduling input based on a target "reconstructTime"
// This includes getting the the most recent baseline and the list of differences. It will return that as a tuple (string, []string)
func GetReconstructionFiles(reconstructTime time.Time) (string, []string) {
	baselineName, baselineTime := GetMostRecentBaseline(reconstructTime)
	differences := GetDifferencesFromBaseline(reconstructTime, baselineTime)
	return baselineName, differences
}

// Function to get the differences between the scheduling input and the baseline

// Based on a passed in time (time.Time), get the most recent Baseline filename from a list of filenames
func GetMostRecentBaseline(reconstructTime time.Time) (string, time.Time) {
	// Get all files in the directory
	files, err := os.ReadDir(logPath)
	if err != nil {
		fmt.Println("Error reading directory:", err)
		return "", time.Time{}
	}

	// Filter out files that don't start with "SchedulingInputBaseline"
	var baselineFiles []string
	regex := regexp.MustCompile(`^SchedulingInputBaseline.*\.log$`)
	for _, file := range files {
		if regex.MatchString(file.Name()) {
			baselineFiles = append(baselineFiles, file.Name())
		}
	}
	// If no baseline files found, return an empty string
	if len(baselineFiles) == 0 {
		return "", time.Time{}
	}

	// For baseline files that do start with that, after Baseline there should be _ then a timestamp formated as "2006-01-02 15-04-05"
	// Parse each of those timestamps from all these files and make a slice of timestamps
	timestamps := []time.Time{}
	regex = regexp.MustCompile(`_([0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{2}-[0-9]{2}-[0-9]{2})\.log$`)
	// Iterate through the baselineFiles and extract the timestamp from each filename
	for _, baselineFile := range baselineFiles {
		match := regex.FindStringSubmatch(baselineFile)
		if len(match) > 1 {
			timestamp, err := time.Parse("2006-01-02_15-04-05", match[1])
			if err == nil {
				timestamps = append(timestamps, timestamp)
			}
		}
	}

	// sort those timestamps to be oldest to newest
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i].Before(timestamps[j]) })
	mostRecentTimestamp := time.Time{}

	// Find the baseline immediately preceding the timestamp we're looking for.
	// This is the most recent baseline file
	for _, timestamp := range timestamps {
		if timestamp.Before(reconstructTime) || timestamp.Equal(reconstructTime) {
			mostRecentTimestamp = timestamp
		}
	}

	// Convert the most recent timestamp back to a filename
	mostRecentBaselineFilename := fmt.Sprintf("SchedulingInputBaseline_%s.log", mostRecentTimestamp.Format("2006-01-02_15-04-05"))

	// Return the most recent baseline filename
	return mostRecentBaselineFilename, mostRecentTimestamp
}

// Similar to the above get most recent baseline, this function will get the most recent scheduling input differences.
// The differences here those is that this is a slice of differences up to and including the SchedulingInputDifference_... file that contains the
// reconstructTime timestamp within it's changes (it could be the first, the last or somewhere in the middle); and that it returns the slice of strings
// of all those filenames.
func GetDifferencesFromBaseline(reconstructTime time.Time, baselineTime time.Time) []string {
	// Get all files in the directory
	files, err := os.ReadDir(logPath)
	if err != nil {
		fmt.Println("Error reading directory:", err)
		return nil
	}

	// Filter out files that don't start with "SchedulingInputDifferences"
	var differenceFiles []string
	regex := regexp.MustCompile(`^SchedulingInputDifferences.*\.log$`)
	for _, file := range files {
		if regex.MatchString(file.Name()) {
			differenceFiles = append(differenceFiles, file.Name())
		}
	}
	// If no difference files found, return an empty string
	if len(differenceFiles) == 0 {
		return nil
	}

	// Map each difference file to a pair of start and end times
	fileTimesMap := map[string][]time.Time{}
	regex = regexp.MustCompile(`_([0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{2}-[0-9]{2}-[0-9]{2})_([0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{2}-[0-9]{2}-[0-9]{2})\.log$`)
	for _, differenceFile := range differenceFiles {
		match := regex.FindStringSubmatch(differenceFile)
		if len(match) > 2 {
			startTime, err := time.Parse("2006-01-02_15-04-05", match[1])
			if err == nil {
				endTime, err := time.Parse("2006-01-02_15-04-05", match[2])
				if err == nil {
					fileTimesMap[differenceFile] = []time.Time{startTime, endTime}
				}
			}
		}
	}

	differenceFilesFromBaseline := []string{}
	for filename, times := range fileTimesMap {
		startTime, _ := times[0], times[1]
		if (startTime.Equal(baselineTime) || startTime.After(baselineTime)) &&
			(reconstructTime.Equal(startTime) || reconstructTime.After(startTime)) {
			differenceFilesFromBaseline = append(differenceFilesFromBaseline, filename)
		}
	}
	return differenceFilesFromBaseline
}

func writeReconstruction(schedulingInput *orb.SchedulingInput, format string) error {
	reconstructedFilename := fmt.Sprintf("ReconstructedSchedulingInput_%s.%s", schedulingInput.Timestamp.Format("2006-01-02_15-04-05"), format)
	var data []byte
	var err error

	switch format {
	case "json":
		data, err = json.Marshal(schedulingInput)
	case "yaml":
		data, err = yaml.Marshal(schedulingInput)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
	if err != nil {
		fmt.Printf("Error marshaling scheduling input to %s: %s\n", format, err)
		return err
	}

	path := filepath.Join(logPath, reconstructedFilename)
	file, err := os.Create(path)
	if err != nil {
		fmt.Println("Error creating file:", err)
		return err
	}
	defer file.Close()

	_, err = file.Write(data)
	if err != nil {
		fmt.Println("Error writing reconstruction to file:", err)
		return err
	}

	fmt.Printf("Reconstruction written to %s file successfully!\n", format)
	return nil
}

// Reconstructs the scheduling input from a log file in PV or local folder
func ReconstructSchedulingInput(fileName string) (*orb.SchedulingInput, error) {
	readdata, err := ReadLog(fileName)
	if err != nil {
		fmt.Println("Error reading from PV:", err)
		return nil, err
	}
	si, err := orb.UnmarshalSchedulingInput(readdata)
	if err != nil {
		fmt.Println("Error converting PB to SI:", err)
		return nil, err
	}
	return si, nil
}

// Function for reconstructing inputs' differences
func ReconstructDifferences(fileNames []string) ([]*orb.SchedulingInputDifferences, error) {
	allDifferences := []*orb.SchedulingInputDifferences{}
	for _, filename := range fileNames {
		readdata, err := ReadLog(filename)
		if err != nil {
			fmt.Println("Error reading from PV:", err)
			return nil, err
		}
		differences, err := orb.UnmarshalBatchedDifferences(readdata)
		if err != nil {
			fmt.Println("Error converting PB to SI:", err)
			return nil, err
		}
		allDifferences = append(allDifferences, differences...)
	}
	return allDifferences, nil
}

// Reads in a NodePools.yaml file and unmarshals into a NodePool slice
func unmarshalNodePoolsFromUser(nodepoolYamlFilepath string) ([]*v1.NodePool, error) {
	yamlFile, err := os.Open(nodepoolYamlFilepath)
	if err != nil {
		fmt.Println("Error opening file:", err)
		return nil, err
	}
	defer yamlFile.Close()

	yamlData, err := io.ReadAll(yamlFile)
	if err != nil {
		fmt.Println("Error reading yaml file:", err)
		return nil, err
	}

	nodePools := []*v1.NodePool{}
	nodepoolsbytes := bytes.Split(yamlData, []byte("\n---\n"))
	for _, nodepoolbytes := range nodepoolsbytes {
		if len(bytes.TrimSpace(nodepoolbytes)) == 0 {
			continue
		}
		nodePool := &v1.NodePool{}
		err = yaml.Unmarshal(nodepoolbytes, nodePool)
		if err != nil {
			return nil, err
		}
		nodePools = append(nodePools, nodePool)
	}

	return nodePools, nil
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

// Defined on aliased type to allow YAML output
func (pe PodErrors) MarshalJSON() ([]byte, error) {
	// Convert the map to a list of structs for serialization
	pairs := []struct {
		Key   string
		Value string
	}{}

	for k, v := range pe {
		pairs = append(pairs, struct {
			Key   string
			Value string
		}{k.String(), v.Error()})
	}
	return json.Marshal(pairs)
}

// Export the results object as a yaml
func printResults(results scheduling.Results, timestamp time.Time, nodePools []*v1.NodePool) {
	podErrors := PodErrors{}
	for pod, err := range results.PodErrors {
		podErrors[pod] = err
	}

	// Map nodePoolNames to their NodePool for faster lookup below
	nodePoolMap := map[string]*v1.NodePool{}
	for _, nodePool := range nodePools {
		nodePoolMap[nodePool.Name] = nodePool
	}

	// Get the truncated and sorted provisionable NodeClaims from the full set of scheduling.NodeClaims
	nodeClaims := []*v1.NodeClaim{}
	for i := range results.NewNodeClaims {
		nodeClaims = append(nodeClaims, results.NewNodeClaims[i].ToNodeClaim(nodePoolMap[results.NewNodeClaims[i].NodePoolName]))
	}

	provisionableResults := struct {
		NewNodeClaims []*v1.NodeClaim
		ExistingNodes []*scheduling.ExistingNode
		PodErrors     PodErrors
	}{
		NewNodeClaims: nodeClaims,
		ExistingNodes: results.ExistingNodes,
		PodErrors:     podErrors,
	}

	yamlData, err := yaml.Marshal(provisionableResults)
	if err != nil {
		fmt.Println("Error converting results to yaml:", err)
		return
	}

	resultsPath := filepath.Join(logPath, fmt.Sprintf("ResimulatedResults_%s.yaml", timestamp.Format("2006-01-02_15-04-05")))
	yamlFile, err := os.Create(resultsPath)
	if err != nil {
		fmt.Println("Error creating results.yaml file:", err)
		return
	}
	_, err = yamlFile.Write(yamlData)
	if err != nil {
		fmt.Println("Error writing results to file:", err)
		return
	}
	fmt.Println("Results written to yaml file successfully!")

}
