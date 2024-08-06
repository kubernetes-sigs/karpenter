# Observability and Reliability Batcher (ORB) Controller

The observability and reliability batcher (ORB) controller, when enabled, logs inputs to Karpenter's provisioner scheduler and
the metadata action that prompted the scheduling (normal provisioning, consolidation and drift) to a mounted Persistent Volume
during Karpenter's runtime.

The associated ORB Debugging Tool is a command-line tool that operates separately from Karpenter runtime and: 
1. Allows the user to reconstruct the original scheduling inputs from the logs as either yaml, json, or both.
1. Allows the user to resimulate the provisioner's scheduling on any scheduling actions logged to verify outputs. 
2. In combination with their favorite debugger, allows the user to step through the resimulation process for closer inspection.

## Table of Contents
- [Setup](setup)
- [Running ORB](#runningORB)
- [Running the ORB Debugging Tool](#runningORBDebuggingTool)
- [Example](#example)
- [License](#license)

<a id="setup"></a>
## Setup

<a id="prereqs"></a>
### Prerequisites

<!-- Note: Needed only if/until development PR approved and merged in -->
1. Pull developmental ORB branches [upstream](https://github.com/LPetro/karpenter/tree/orbpoc) and downstream:
   
    - AWS: [Karpenter-provider-AWS with ORB](https://github.com/LPetro/karpenter-provider-aws/tree/orblog)  
        Note: While in development, if you are trying to test local branch edits to upstream, you will need to commit them in order to pull the changes into the Go workspace upstream. To do so:
        - Navigate to your karpenter-provider-aws directory  
        `cd /path/to/your/local/karpenter-provider-aws`

        - Replace the karpenter dependency with the latest commit  
        `go mod edit -replace sigs.k8s.io/karpenter=github.com/path/to/your/upstream/changes/karpenter@"$commit_hash"`

        - Tidy the go modules  
        `go mod tidy`

        - Apply the changes  
        `make apply`  

1. [Setup Karpenter](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)
 
2. Create a Persistent Volume (PV).  
   
    - AWS: Create a [general-purpose S3 Bucket](https://docs.aws.amazon.com/AmazonS3/latest/userguide/creating-bucket.html).
    <!-- Kwok: (TODO: is there a template I could use here?) -->
3. Mount the PV to your cluster.  
   
    - AWS: Use the [Mountpoint for Amazon S3 CSI Driver](https://docs.aws.amazon.com/eks/latest/userguide/s3-csi.html), configured with your S3 bucket name from above.
4. Create a static provisioning yaml file for your mounted PV/PVC.  
    
    - AWS: [S3 CSI Driver Static Provisioning Example](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/examples/kubernetes/static_provisioning/README.md)  
    
        Example:
        ```yaml
        apiVersion: v1
        kind: PersistentVolume
        metadata:
        name: s3-pv
        namespace: kube-system
        spec:
        capacity:
            storage: 1200Gi # ignored, required
        accessModes:
            - ReadWriteMany
        mountOptions:
            - allow-delete
            - region us-west-2
        csi:
            driver: s3.csi.aws.com # required
            volumeHandle: s3-csi-driver-volume
            volumeAttributes:
            bucketName: examplebucketname
        ---
        apiVersion: v1
        kind: PersistentVolumeClaim
        metadata:
        name: s3-claim
        namespace: kube-system
        spec:
        accessModes:
            - ReadWriteMany # supported options: ReadWriteMany / ReadOnlyMany
        storageClassName: "" # required for static provisioning
        resources:
            requests:
            storage: 1200Gi # ignored, required
        volumeName: s3-pv
        ```  
5. Deploy the PV/PVC static configuration:  
   
   `kubectl apply -f path/to/pv_and_pvc_static_configuration.yaml`

<!-- TODO: Make a note/warning for the mount location field, if they change from \data, I need to make a cli option to change this for the tool. 

This could just be a description of what needs to be added/changed in the deployment.yaml for compatibility. i.e. the portion with:
    - name: persistent-storage
     mountPath: /data
-->   

<a id="runningORB"></a>
## Running ORB
The following section assumes you have met all the [prerequisites](#prereqs): Karpenter is up and running with the PV you have created and mounted to your cluster.
<!-- TODO: Add feature-flag in. Include here the instructions for how to enable/disable feature. -->
Once Karpenter is running, it will automatically log all provisioning scheduling data every reconcile cycle to your mounted PV as:  
-  `SchedulingInputBaseline_`\<timestamp\>`.log`,  
- `SchedulingInputDifferences_`\<starting-timestamp\>`_`\<ending-timestamp\>`.log`, and  
- `SchedulingMetadata_`\<starting-timestamp\>`_`\<ending-timestamp\>`.log`

<a id="runningORBDebuggingTool"></a>
## Running the ORB Debugging Tool


<!-- TODO: make a note about /data default directory. If I make an arg for the tool, find a way to change this for a Karpenter run... perhaps with the same process that I'd use to feature-flag / enable/disable it... -->
To run the ORB debugging tool:  
1. Download logs from your PV and save them to the sample_logs folder `karpenter/hack/orbdebuggingtool/sample_logs` or a folder of your choice.  
   This will be the target directory for the `-dir` flag.
  
    Note: For whichever time period you're looking to inspect, ensure you have <u>at least</u>: (1) the baseline preceding that time period and (2) all the differences and (3) metadata files from that baseline through the desired resimulation time. This is critical to the reconstruction and resimulation logic; not having these could lead to undefined behavior.
<!-- The reconstruction takes a baseline and merges in all differences additively. If any are missing, it could lead to undefined behavior. -->
2. Find your nodepool configuration file. If part of a larger yaml config file, create a new nodepools.yaml of just the nodepool information. This will be the target directory for the `-yaml` flag.
3. `cd ./path/to/karpenterfork/hack/orbdebuggingtool`
4. `go run main.go -dir /path/to/sample_logs -nodepools /path/to/nodepools.yaml`  
   
  <a id="example"></a>
## Example
 `go run main.go -dir ./sample_logs -nodepools ./nodepools.yaml -reconstruct=true`  
  
This will find all metadata info in the provided sample logs, combine and sort them to provide them as options to the user as shown below.

```bash
orbdebuggingtool % go run main.go -dir ./sample_logs -nodepools ./nodepools.yaml -reconstruct=true
Available options:
0. normal-provisioning (2024-08-05_18-18-45)
1. normal-provisioning (2024-08-05_18-18-55)
2. normal-provisioning (2024-08-05_18-19-00)
3. normal-provisioning (2024-08-05_18-19-03)
4. normal-provisioning (2024-08-05_18-19-06)
5. normal-provisioning (2024-08-05_18-19-10)
6. normal-provisioning (2024-08-05_18-19-12)
7. normal-provisioning (2024-08-05_18-19-20)
8. normal-provisioning (2024-08-05_18-19-30)
9. single-node-consolidation (2024-08-05_18-19-43)
10. single-node-consolidation (2024-08-05_18-19-53)
11. single-node-consolidation (2024-08-05_18-22-11)
12. normal-provisioning (2024-08-05_19-18-03)
13. normal-provisioning (2024-08-05_19-18-13)
14. normal-provisioning (2024-08-05_19-18-23)
15. normal-provisioning (2024-08-05_19-18-33)
16. single-node-consolidation (2024-08-05_19-18-58)
Enter the option number: 
```
  
Enter the desired scheduling action you'd like to reconstruct or resimulate

```bash
orbdebuggingtool % go run main.go -dir ./sample_logs -nodepools ./nodepools.yaml -reconstruct=true
Available options:
0. normal-provisioning (2024-08-05_18-18-45)
1. normal-provisioning (2024-08-05_18-18-55)
2. normal-provisioning (2024-08-05_18-19-00)
3. normal-provisioning (2024-08-05_18-19-03)
4. normal-provisioning (2024-08-05_18-19-06)
5. normal-provisioning (2024-08-05_18-19-10)
6. normal-provisioning (2024-08-05_18-19-12)
7. normal-provisioning (2024-08-05_18-19-20)
8. normal-provisioning (2024-08-05_18-19-30)
9. single-node-consolidation (2024-08-05_18-19-43)
10. single-node-consolidation (2024-08-05_18-19-53)
11. single-node-consolidation (2024-08-05_18-22-11)
12. normal-provisioning (2024-08-05_19-18-03)
13. normal-provisioning (2024-08-05_19-18-13)
14. normal-provisioning (2024-08-05_19-18-23)
15. normal-provisioning (2024-08-05_19-18-33)
16. single-node-consolidation (2024-08-05_19-18-58)
Enter the option number: 16

Selected option: '16. single-node-consolidation (2024-08-05_19-18-58)'
Finding baseline file:  SchedulingInputBaseline_2024-08-05_19-18-13.log
Reconstruction written to json file successfully!
Reconstruction written to yaml file successfully!
Results written to yaml file successfully!
```

### Usage
- `-all`: Reconstruct and/or resimulate *all* scheduling inputs for samples provided
- `-dir string`: Path to the directory containing logs
- `-out string`: Output directory for reconstructed scheduling input files (default ".")
- `-rec-output string`: Output format for reconstructed scheduling input (yaml, json, or both) (default "yaml")
- `-reconstruct`: Reconstruct scheduling input(s) and print reconstruction to file. This is much slower than just resimulation.
- `-resimulate`: Reconstruct and resimulate scheduling input(s).
Setting reconstruct to false but resimulate to true will reconstruct for resimulation but not print out the reconstruction. (default true)
- `-nodepools string`: Path to the YAML file containing NodePool definitions


<a id="license"></a>
## License

Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.