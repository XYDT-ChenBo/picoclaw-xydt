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
	start := time.Now()
	log.Printf("go_node: camera.snap start")
	defer func() {
		log.Printf("go_node: camera.snap done, totalMs=%d", time.Since(start).Milliseconds())
	}()

	// 1. snapshotSrc 目录来自 config.json 的 exec.workDir，文件名按时间命名
	stepStart := time.Now()
	snapshotSrc := h.buildSnapshotPath()
	if err := os.MkdirAll(filepath.Dir(snapshotSrc), 0o755); err != nil {
		log.Printf("go_node: camera.snap error stage=create_workDir dir=%s err=%v elapsedMs=%d", filepath.Dir(snapshotSrc), err, time.Since(start).Milliseconds())
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: create workDir %s: %v", filepath.Dir(snapshotSrc), err),
			},
		}
	}
	log.Printf("go_node: camera.snap stage=create_workDir_ok dir=%s elapsedMs=%d", filepath.Dir(snapshotSrc), time.Since(stepStart).Milliseconds())
	width, height := 640, 360

	cmdStart := time.Now()
	cmd := exec.Command("sendcmd", "avserver..snapshot", "start", snapshotSrc, "640", "360")
	if err := cmd.Run(); err != nil {
		log.Printf("go_node: camera.snap error stage=sendcmd err=%v elapsedMs=%d", err, time.Since(cmdStart).Milliseconds())
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: exec sendcmd failed: %v", err),
			},
		}
	}
	log.Printf("go_node: camera.snap stage=sendcmd_ok snapshotPath=%s elapsedMs=%d", snapshotSrc, time.Since(cmdStart).Milliseconds())

	// 2. snapshotSrc 已位于 exec.workDir，直接读取即可
	destPath := snapshotSrc

	// 3. 从 exec.workDir 下读取图片文件并返回
	// 某些设备上 sendcmd 返回后文件落盘可能存在轻微延迟，这里增加短暂重试以提高稳定性。
	var (
		data     []byte
		readErr  error
		attempts int
	)
	readStart := time.Now()
	for i := 0; i < 20; i++ {
		attempts = i + 1
		data, readErr = os.ReadFile(destPath)
		if readErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if readErr != nil {
		log.Printf("go_node: camera.snap error stage=read_file path=%s attempts=%d elapsedMs=%d err=%v", destPath, attempts, time.Since(readStart).Milliseconds(), readErr)
		return gateway.InvokeResult{
			OK: false,
			Error: &gateway.ErrorShape{
				Code:    "UNAVAILABLE",
				Message: fmt.Sprintf("camera.snap: read file %s: %v", destPath, readErr),
			},
		}
	}
	log.Printf("go_node: camera.snap stage=read_file_ok path=%s attempts=%d elapsedMs=%d", destPath, attempts, time.Since(readStart).Milliseconds())

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(destPath), "."))
	if ext == "" {
		ext = "jpg"
	}
	if ext == "jpeg" {
		ext = "jpg"
	}

	encodeStart := time.Now()
	b64 := base64.StdEncoding.EncodeToString(data)
	encodeMs := time.Since(encodeStart).Milliseconds()
	totalMs := time.Since(start).Milliseconds()
	log.Printf("go_node: camera.snap done path=%s bytes=%d width=%d height=%d encodeMs=%d totalMs=%d", destPath, len(data), width, height, encodeMs, totalMs)

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
