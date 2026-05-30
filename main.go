package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

const (
	ConfigFile = "./config.json"
	LedgerFile = "./managed_files.json"
)

type Config struct {
	ServerURL string `json:"server_url"`
	ModpackID string `json:"modpack_id"`
	ModsDir   string `json:"mods_dir"`
}

type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Version int         `json:"version"`
	Files   []FileEntry `json:"files"`
}

type LocalLedger struct {
	LastSyncVersion int         `json:"last_synchronized_version"`
	DownloadedFiles []FileEntry `json:"downloaded_files"`
}

func main() {
	a := app.New()
	w := a.NewWindow("MC-Depsync 模组同步器")
	w.Resize(fyne.NewSize(550, 380))

	cfg := loadConfig()

	serverURLEntry := widget.NewEntry()
	serverURLEntry.SetText(cfg.ServerURL)
	serverURLEntry.SetPlaceHolder("例如: http://localhost:8080")

	modpackIDEntry := widget.NewEntry()
	modpackIDEntry.SetText(cfg.ModpackID)
	modpackIDEntry.SetPlaceHolder("请在此处粘贴的整合包 UUID")

	modsDirEntry := widget.NewEntry()
	modsDirEntry.SetText(cfg.ModsDir)
	modsDirEntry.SetPlaceHolder("默认: ./mods")

	statusLabel := widget.NewLabel("状态: 准备就绪")
	progressBar := widget.NewProgressBar()

	form := widget.NewForm(
		widget.NewFormItem("服务器地址", serverURLEntry),
		widget.NewFormItem("整合包 UUID", modpackIDEntry),
		widget.NewFormItem("模组存放目录", modsDirEntry),
	)

	var syncBtn *widget.Button
	syncBtn = widget.NewButton("保存配置并同步模组", func() {
		if modpackIDEntry.Text == "" {
			dialog.ShowInformation("提示", "请输入有效的整合包 UUID！", w)
			return
		}

		currentCfg := Config{
			ServerURL: serverURLEntry.Text,
			ModpackID: modpackIDEntry.Text,
			ModsDir:   modsDirEntry.Text,
		}

		saveConfig(currentCfg)

		// 防抖
		syncBtn.Disable()
		serverURLEntry.Disable()
		modpackIDEntry.Disable()
		modsDirEntry.Disable()
		progressBar.SetValue(0)

		go func() {
			defer func() {
				syncBtn.Enable()
				serverURLEntry.Enable()
				modpackIDEntry.Enable()
				modsDirEntry.Enable()
			}()
			runSyncProcess(currentCfg, statusLabel, progressBar, w)
		}()
	})

	title := widget.NewLabelWithStyle("MC-Depsync 模组同步器", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	content := container.NewVBox(
		title,
		widget.NewLabel("请输入服主提供的配置信息："),
		form,
		widget.NewSeparator(),
		statusLabel,
		progressBar,
		widget.NewLabel(" "),
		syncBtn,
	)

	w.SetContent(container.NewPadded(content))
	w.ShowAndRun()
}

func runSyncProcess(cfg Config, status *widget.Label, progress *widget.ProgressBar, w fyne.Window) {
	os.MkdirAll(cfg.ModsDir, 0755)
	ledger := loadLocalLedger()

	status.SetText("状态: 正在连接云端服务器...")
	remoteManifest, err := fetchRemoteManifest(cfg)
	if err != nil {
		dialog.ShowError(fmt.Errorf("云端连接失败: %v", err), w)
		status.SetText("状态: 同步终止")
		return
	}

	status.SetText("状态: 正在校验本地文件完整性...")
	localMap := scanLocalModsFast(cfg.ModsDir)

	status.SetText("状态: 正在进行三方差量比对...")
	toDownload, toRename, toDelete := reconcile(remoteManifest, ledger, localMap)

	if ledger.LastSyncVersion == remoteManifest.Version && len(toDownload) == 0 && len(toRename) == 0 && len(toDelete) == 0 {
		status.SetText(fmt.Sprintf("状态: 已是最新版 (v%d)，完整性校验通过！", remoteManifest.Version))
		progress.SetValue(1.0)
		dialog.ShowInformation("提示", "您的模组已经是最新且完整的官方状态, 可以启动游戏！", w)
		return
	}

	executeLocalFileOps(cfg.ModsDir, toRename, toDelete)

	if len(toDownload) > 0 {
		status.SetText(fmt.Sprintf("状态: 发现文件变动或缺失, 开始下载 %d 个模组...", len(toDownload)))
		executeDownloadsWithProgress(cfg, toDownload, status, progress)
	} else {
		progress.SetValue(1.0)
	}

	ledger.LastSyncVersion = remoteManifest.Version
	ledger.DownloadedFiles = remoteManifest.Files
	saveLocalLedger(ledger)

	status.SetText(fmt.Sprintf("状态: 同步成功！当前版本: v%d", remoteManifest.Version))
	dialog.ShowInformation("恭喜", "整合包已同步并修复至服务器最新状态！", w)
}

func reconcile(remote Manifest, ledger LocalLedger, localMap map[string]string) ([]FileEntry, map[string]string, []string) {
	var toDownload []FileEntry
	toRename := make(map[string]string)
	var toDelete []string
	remoteHashes := make(map[string]bool)

	for _, rf := range remote.Files {
		remoteHashes[rf.SHA256] = true
		if localActualPath, exists := localMap[rf.SHA256]; exists {
			if localActualPath != rf.Path {
				toRename[localActualPath] = rf.Path
			}
		} else {
			toDownload = append(toDownload, rf)
		}
	}

	for _, ledgerFile := range ledger.DownloadedFiles {
		if !remoteHashes[ledgerFile.SHA256] {
			if actualLocalPath, exists := localMap[ledgerFile.SHA256]; exists {
				toDelete = append(toDelete, actualLocalPath)
			}
		}
	}
	return toDownload, toRename, toDelete
}

func scanLocalModsFast(modsDir string) map[string]string {
	localMap := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	filesToHash := make(chan string, 100)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range filesToHash {
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				h := sha256.New()
				buf := make([]byte, 64*1024)
				io.CopyBuffer(h, f, buf)
				f.Close()

				hashStr := hex.EncodeToString(h.Sum(nil))
				relPath, _ := filepath.Rel(filepath.Dir(modsDir), path)
				relPath = filepath.ToSlash(relPath)

				mu.Lock()
				localMap[hashStr] = relPath
				mu.Unlock()
			}
		}()
	}

	filepath.Walk(modsDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			filesToHash <- path
		}
		return nil
	})
	close(filesToHash)
	wg.Wait()
	return localMap
}

func executeLocalFileOps(modsDir string, toRename map[string]string, toDelete []string) {
	for oldPath, newPath := range toRename {
		fullOld := filepath.Join(filepath.Dir(modsDir), oldPath)
		fullNew := filepath.Join(filepath.Dir(modsDir), newPath)
		os.MkdirAll(filepath.Dir(fullNew), 0755)
		os.Rename(fullOld, fullNew)
	}
	for _, delPath := range toDelete {
		fullDel := filepath.Join(filepath.Dir(modsDir), delPath)
		os.Remove(fullDel)
	}
}

func executeDownloadsWithProgress(cfg Config, toDownload []FileEntry, status *widget.Label, progress *widget.ProgressBar) {
	totalFiles := len(toDownload)
	completedFiles := 0
	var mu sync.Mutex

	downloadChan := make(chan FileEntry, totalFiles)
	for _, f := range toDownload {
		downloadChan <- f
	}
	close(downloadChan)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range downloadChan {
				url := fmt.Sprintf("%s/download/cunz/%s", cfg.ServerURL, file.SHA256)
				targetPath := filepath.Join(filepath.Dir(cfg.ModsDir), file.Path)
				os.MkdirAll(filepath.Dir(targetPath), 0755)

				resp, err := http.Get(url)
				if err == nil && resp.StatusCode == 200 {
					out, _ := os.Create(targetPath)
					io.Copy(out, resp.Body)
					out.Close()
					resp.Body.Close()
				}

				mu.Lock()
				completedFiles++
				progress.SetValue(float64(completedFiles) / float64(totalFiles))
				status.SetText(fmt.Sprintf("状态: 正在下载模组... (%d/%d)", completedFiles, totalFiles))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func fetchRemoteManifest(cfg Config) (Manifest, error) {
	var m Manifest
	url := fmt.Sprintf("%s/api/modpacks/%s/manifest/latest", cfg.ServerURL, cfg.ModpackID)
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return m, fmt.Errorf("状态码 %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&m)
	return m, nil
}

func loadConfig() Config {
	var cfg Config
	cfg.ServerURL = "http://localhost:8080"
	cfg.ModsDir = "./mods"

	data, err := os.ReadFile(ConfigFile)
	if err == nil {
		json.Unmarshal(data, &cfg)
	}
	return cfg
}

func saveConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(ConfigFile, data, 0644)
}

func loadLocalLedger() LocalLedger {
	var ledger LocalLedger
	data, err := os.ReadFile(LedgerFile)
	if err == nil {
		json.Unmarshal(data, &ledger)
	}
	return ledger
}

func saveLocalLedger(ledger LocalLedger) {
	data, _ := json.MarshalIndent(ledger, "", "  ")
	os.WriteFile(LedgerFile, data, 0644)
}
