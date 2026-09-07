Forked from https://github.com/k0sproject/k0smotron@v2.1.0 / 62231f3adc8cbc313622d8696f783e38fa1137d2 to prevent an import of k0smotron as library to:

1. Use strong types for k0smotron objects.
2. Avoid adding a large number of dependencies to the go.mod. Importing the module raises the
   required Go version to 1.26 and pulls `tablewriter` to v1, whose API `dpfctl` does not use.
3. Allow the DPF operator to keep Kubernetes library versions independent of the k0smotron versions.

Only the standalone `k0smotron.io` kinds are forked. The Cluster API groups are unused, as is the
webhook, so both are dropped and deepcopy is regenerated rather than copied.

Copying the API was done via `hack/scripts/go-sync-third-party-forks.sh`.
