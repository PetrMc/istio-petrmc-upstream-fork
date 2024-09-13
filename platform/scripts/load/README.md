# Peering load testing

Usage:

```shell
# Update values.yaml to your image
source platform/scripts/load/tools.sh
# 0..N is the count of clusters
load::deploy-many {0..2}
load::link-many {0..2}
# Count of services
load::create-services 2 {0..2}
```
