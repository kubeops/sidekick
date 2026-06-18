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

package grpc

import "encoding/json"

type SnapShot struct {
	LogInfo      LogInfo `json:"logInfo"`
	SidekickName string  `json:"sidekickName"`
	NameSpace    string  `json:"namespace"`
	Token        string  `json:"token"`
	// SnapshotToken is the operator-issued, encrypted grant naming the single
	// Snapshot this archiver is authorized to update. The archiver cannot read or
	// forge it (it lacks the snapshot encryption key) — it only forwards it. The
	// server decrypts it with the per-Sidekick snapshot key and refuses to act on
	// any Snapshot other than the one named inside.
	SnapshotToken string `json:"snapshotToken"`
}

// BindingPayload returns the canonical bytes that a token is bound to: every
// request-identifying field except the token itself. The minting client and the
// verifying server both feed this into token.RequestDigest, so a token can only
// be used with the exact payload it was minted for. Marshalling a struct with
// fixed field order keeps the output stable across both sides.
func (s SnapShot) BindingPayload() ([]byte, error) {
	return json.Marshal(struct {
		SidekickName string  `json:"sidekickName"`
		NameSpace    string  `json:"namespace"`
		LogInfo      LogInfo `json:"logInfo"`
	}{
		SidekickName: s.SidekickName,
		NameSpace:    s.NameSpace,
		LogInfo:      s.LogInfo,
	})
}

type LogInfo struct {
	Type      string `json:"type"`
	Log       string `json:"log"`
	LogLimit  int    `json:"logLimit"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}
