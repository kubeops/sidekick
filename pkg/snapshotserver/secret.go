/*
Copyright AppsCode Inc. and Contributors.

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

package snapshotserver

const (
	// SigningSecretKey / SnapshotSecretKey are the data keys under which the
	// per-Sidekick token signing key and snapshot-name encryption key live in the
	// Secret. The reconciler (apps) writes them; the gRPC server reads them.
	SigningSecretKey  = "signing-key"
	SnapshotSecretKey = "snapshot-key"
)

// SigningSecretName is the name of the Secret holding a Sidekick's gRPC keys.
func SigningSecretName(sidekickName string) string {
	return sidekickName + "-grpc-key"
}
