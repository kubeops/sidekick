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
