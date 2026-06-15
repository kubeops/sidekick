package grpc

type SnapShot struct {
	Data         []byte `json:"data"`
	SidekickName string `json:"sidekickName"`
	NameSpace    string `json:"namespace"`
	Token        string `json:"token"`
}
