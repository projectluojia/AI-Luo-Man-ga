package packagecontract

// 包格式的统一大小上限，供作者工具、安装器和宿主适配器共同执行。
const (
	MaxManifestBytes = int64(256 << 10)
	MaxLockBytes     = int64(64 << 10)
	MaxArtifactBytes = int64(1 << 30)
)
