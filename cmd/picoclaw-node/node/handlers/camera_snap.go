package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/go_node/config"
	"github.com/openclaw/go_node/gateway"
)

// CameraSnapHandler 处理 camera.snap 指令：
// 1. 执行设备拍照命令：sendcmd avserver..snapshot start <exec.workDir>/snapshot_<time>.jpeg 640 360
// 2. 图片直接保存到 config.json 中 exec.workDir 指定的目录，文件名按时间命名
// 3. 读取图片并以 base64 形式返回
type CameraSnapHandler struct {
	cfg config.ExecConfig
}

// NewCameraSnapHandler 创建拍照处理器
func NewCameraSnapHandler(cfg config.ExecConfig) *CameraSnapHandler {
	return &CameraSnapHandler{cfg: cfg}
}

func (h *CameraSnapHandler) Handle(req gateway.InvokeRequest) gateway.InvokeResult {
	// 1. snapshotSrc 目录来自 config.json 的 exec.workDir，文件名按时间命名
	snapshotSrc := h.buildSnapshotPath()
	if err := os.MkdirAll(filepath.Dir(snapshotSrc), 0o755); err != nil {
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: create workDir %s: %v", filepath.Dir(snapshotSrc), err),
			},
		}
	}
	width, height := 640, 360

	cmd := exec.Command("sendcmd", "avserver..snapshot", "start", snapshotSrc, "640", "360")
	if err := cmd.Run(); err != nil {
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: exec sendcmd failed: %v", err),
			},
		}
	}

	log.Printf("go_node: camera.snap command success, snapshot path=%s", snapshotSrc)

	// 2. snapshotSrc 已位于 exec.workDir，直接读取即可
	destPath := snapshotSrc

	// 3. 从 exec.workDir 下读取图片文件并返回
	// 某些设备上 sendcmd 返回后文件落盘可能存在轻微延迟，这里增加短暂重试以提高稳定性。
	var (
		data    []byte
		readErr error
	)
	for i := 0; i < 20; i++ {
		data, readErr = os.ReadFile(destPath)
		if readErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if readErr != nil {
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: read file %s: %v", destPath, readErr),
			},
		}
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(destPath), "."))
	if ext == "" {
		ext = "jpg"
	}
	if ext == "jpeg" {
		ext = "jpg"
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	log.Printf("go_node: camera.snap path=%s bytes=%d width=%d height=%d", destPath, len(data), width, height)

	out := map[string]any{
		"format": ext,
		"base64": b64,
		"width":  width,
		"height": height,
	}
	payload, _ := json.Marshal(out)
	return gateway.InvokeResult{
		OK:          true,
		PayloadJSON: string(payload),
	}
}

// buildSnapshotPath 返回位于 exec.workDir 目录下的照片路径，文件名按时间命名
func (h *CameraSnapHandler) buildSnapshotPath() string {
	workDir := strings.TrimSpace(h.cfg.WorkDir)
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	if workDir == "" {
		workDir = "."
	}
	filename := fmt.Sprintf("snapshot_%s.jpeg", time.Now().Format("20060102_150405"))
	return filepath.Join(workDir, filename)
}
