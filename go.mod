// 模块路径：目前不带域名，clone 下来直接 go build 即可。
//
// 若想让别人能 `go install github.com/<owner>/orcabridge@latest`，
// 请改成你的仓库地址（模块路径必须与实际仓库 URL 一致），例如：
//
//	module github.com/OWNER/orcabridge
//
// 改完记得同步 README 里的 go build 示例。
module orcabridge

go 1.21
