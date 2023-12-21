# Kwok Provider

Before using the kwok provider, make sure that you don't have an installed version of Karpenter in your cluster. 

## Requirements
- Have a repository that you can build, push, and pull images from.
- 

## Installing
```bash
make install-kwok
make apply
```

## Uninstalling
```bash
helm uninstall karpenter -n karpenter
make uninstall-kwok
```

To use the kwok provider, you'll need to first run `make install-kwok`. 
Then run `make apply`, and then you should be good to go.

