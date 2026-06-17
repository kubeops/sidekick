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

type SnapShot struct {
	LogInfo      LogInfo `json:"logInfo"`
	SidekickName string  `json:"sidekickName"`
	NameSpace    string  `json:"namespace"`
	Token        string  `json:"token"`
}

type LogInfo struct {
	Type      string `json:"type"`
	Log       string `json:"log"`
	LogLimit  int    `json:"logLimit"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}
