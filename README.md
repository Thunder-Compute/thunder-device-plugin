# Thunder Device Plugin

## Components

1. Operator

    - Reconciles the number of available vGPU devices in the cluster, based on live API data from the Thunder Compute API.

2. Daemonset

    - Provisions nodes with Thunder Compute drivers
    - Monitors node health periodically
    - Serves the Dynamic Resource Allocation methods to prepare ResourceClaims
    - Serves the CDI for vGPUs
    - Controls `client.thundercompute.com` resources to persist information about the clients
    