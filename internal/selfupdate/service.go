package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultHTTPTimeoutSeconds  = 20
	selfUpdateUserAgent        = "CLIProxyAPI-self-update"
	maxSelfUpdateDownloadSize  = 512 << 20 // 512 MB
	selfUpdateStatusTimeFormat = time.RFC3339
)

var errSelfUpdateFound = errors.New("self-update found")

type Status struct {
	Enabled          bool   `json:"enabled"`
	Repository       string `json:"repository,omitempty"`
	CurrentVersion   string `json:"current_version"`
	CurrentCommit    string `json:"current_commit,omitempty"`
	CurrentBuildDate string `json:"build_date,omitempty"`
	LatestVersion    string `json:"latest_version,omitempty"`
	UpdateAvailable  bool   `json:"update_available,omitempty"`
	LastCheckAt      string `json:"last_check_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	InProgress       bool   `json:"in_progress,omitempty"`
	PendingRestart   bool   `json:"pending_restart,omitempty"`
	RunningInDocker  bool   `json:"running_in_docker"`
	AssetName        string `json:"asset_name,omitempty"`
}

type Service struct {
	mu             sync.Mutex
	lastRelease    *releaseInfo
	lastAsset      *releaseAsset
	lastCheckAt    time.Time
	lastError      string
	checking       bool
	updating       bool
	pendingRestart bool
	configPath     string
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type releaseInfo struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	HtmlURL     string         `json:"html_url"`
	Body        string         `json:"body"`
	PublishedAt string         `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

type configSnapshot struct {
	Enabled             bool
	Repository          string
	GitHubToken         string
	ProxyURL            string
	AssetPrefix         string
	WorkDir             string
	RestartDelaySeconds int
	ExecutableName      string
}

func NewService(configPath string) *Service {
	return &Service{configPath: strings.TrimSpace(configPath)}
}

func (s *Service) Status(cfg *config.Config) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildStatusLocked(cfg)
}

func (s *Service) Check(ctx context.Context, cfg *config.Config) Status {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := resolveConfig(cfg, s.configPath)
	if !snapshot.Enabled {
		s.mu.Lock()
		s.lastError = "自更新未启用"
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	if snapshot.Repository == "" {
		s.mu.Lock()
		s.lastError = "未配置更新仓库"
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}

	s.mu.Lock()
	if s.checking || s.updating {
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	s.checking = true
	s.mu.Unlock()

	release, asset, err := fetchLatestRelease(ctx, snapshot)

	s.mu.Lock()
	s.checking = false
	s.lastCheckAt = time.Now().UTC()
	if err != nil {
		s.lastError = err.Error()
		s.lastRelease = nil
		s.lastAsset = nil
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}

	s.lastError = ""
	s.lastRelease = release
	s.lastAsset = asset
	status := s.buildStatusLocked(cfg)
	s.mu.Unlock()
	return status
}

func (s *Service) Confirm(ctx context.Context, cfg *config.Config) Status {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := resolveConfig(cfg, s.configPath)
	if !snapshot.Enabled {
		s.mu.Lock()
		s.lastError = "自更新未启用"
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	if snapshot.Repository == "" {
		s.mu.Lock()
		s.lastError = "未配置更新仓库"
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	if runtime.GOOS == "windows" {
		s.mu.Lock()
		s.lastError = "Windows 运行中无法安全替换二进制，请手动更新"
		status := s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}

	status := s.Check(ctx, cfg)
	if status.LastError != "" {
		return status
	}

	s.mu.Lock()
	if s.updating {
		status = s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	asset := s.lastAsset
	release := s.lastRelease
	if asset == nil || release == nil {
		s.lastError = "未找到匹配当前平台的更新包"
		status = s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	latest := resolveLatestVersion(release)
	if latest == "" {
		s.lastError = "最新版本信息为空"
		status = s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	if !hasNewerVersion(buildinfo.Version, latest) {
		s.lastError = "当前已是最新版本"
		status = s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	s.updating = true
	s.mu.Unlock()

	err := applyUpdate(ctx, snapshot, asset)

	s.mu.Lock()
	s.updating = false
	if err != nil {
		s.lastError = err.Error()
		status = s.buildStatusLocked(cfg)
		s.mu.Unlock()
		return status
	}
	s.lastError = ""
	s.pendingRestart = true
	status = s.buildStatusLocked(cfg)
	s.mu.Unlock()

	scheduleRestart(snapshot.RestartDelaySeconds)
	return status
}

func (s *Service) buildStatusLocked(cfg *config.Config) Status {
	snapshot := resolveConfig(cfg, s.configPath)
	status := Status{
		Enabled:          snapshot.Enabled,
		Repository:       snapshot.Repository,
		CurrentVersion:   buildinfo.Version,
		CurrentCommit:    buildinfo.Commit,
		CurrentBuildDate: buildinfo.BuildDate,
		LastError:        strings.TrimSpace(s.lastError),
		InProgress:       s.checking || s.updating,
		PendingRestart:   s.pendingRestart,
		RunningInDocker:  isRunningInDocker(),
	}
	if s.lastRelease != nil {
		status.LatestVersion = resolveLatestVersion(s.lastRelease)
		if status.LatestVersion != "" {
			status.UpdateAvailable = hasNewerVersion(buildinfo.Version, status.LatestVersion)
		}
	}
	if !s.lastCheckAt.IsZero() {
		status.LastCheckAt = s.lastCheckAt.Format(selfUpdateStatusTimeFormat)
	}
	if s.lastAsset != nil {
		status.AssetName = s.lastAsset.Name
	}
	return status
}

func resolveConfig(cfg *config.Config, configPath string) configSnapshot {
	snapshot := configSnapshot{}
	if cfg != nil {
		snapshot.Enabled = cfg.RemoteManagement.SelfUpdateEnabled
		snapshot.Repository = strings.TrimSpace(cfg.RemoteManagement.SelfUpdateRepository)
		snapshot.GitHubToken = strings.TrimSpace(cfg.RemoteManagement.SelfUpdateGitHubToken)
		snapshot.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
		snapshot.AssetPrefix = strings.TrimSpace(cfg.RemoteManagement.SelfUpdateAssetPrefix)
		snapshot.WorkDir = strings.TrimSpace(cfg.RemoteManagement.SelfUpdateWorkDir)
		snapshot.RestartDelaySeconds = cfg.RemoteManagement.SelfUpdateRestartDelaySeconds
		snapshot.ExecutableName = strings.TrimSpace(cfg.RemoteManagement.SelfUpdateExecutableName)
	}

	if snapshot.Repository == "" {
		snapshot.Repository = strings.TrimSpace(os.Getenv("APP_UPDATE_REPOSITORY"))
	}
	if snapshot.GitHubToken == "" {
		snapshot.GitHubToken = strings.TrimSpace(os.Getenv("UPDATE_GITHUB_TOKEN"))
	}
	if snapshot.AssetPrefix == "" {
		snapshot.AssetPrefix = config.DefaultSelfUpdateAssetPrefix
	}
	if snapshot.WorkDir == "" {
		snapshot.WorkDir = config.DefaultSelfUpdateWorkDir
	}
	if snapshot.RestartDelaySeconds <= 0 {
		snapshot.RestartDelaySeconds = config.DefaultSelfUpdateRestartDelaySeconds
	}

	if snapshot.WorkDir != "" && !filepath.IsAbs(snapshot.WorkDir) {
		baseDir := ""
		if configPath != "" {
			baseDir = filepath.Dir(configPath)
		}
		if baseDir == "" {
			if wd, err := os.Getwd(); err == nil {
				baseDir = wd
			}
		}
		if baseDir != "" {
			snapshot.WorkDir = filepath.Join(baseDir, snapshot.WorkDir)
		}
	}

	return snapshot
}

func fetchLatestRelease(ctx context.Context, snapshot configSnapshot) (*releaseInfo, *releaseAsset, error) {
	releaseURL, err := resolveReleaseURL(snapshot.Repository)
	if err != nil {
		return nil, nil, err
	}
	client := newHTTPClient(snapshot.ProxyURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", selfUpdateUserAgent)
	if snapshot.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+snapshot.GitHubToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("self-update: failed to close release response body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, nil, fmt.Errorf("Release 查询失败: status=%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&payload); errDecode != nil {
		return nil, nil, fmt.Errorf("Release 解析失败: %w", errDecode)
	}
	if strings.TrimSpace(payload.TagName) == "" && strings.TrimSpace(payload.Name) == "" {
		return nil, nil, errors.New("Release 返回缺少版本信息")
	}

	asset := pickAsset(payload.Assets, snapshot.AssetPrefix)
	return &payload, asset, nil
}

func resolveLatestVersion(release *releaseInfo) string {
	if release == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(release.TagName); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(release.Name)
}

func resolveReleaseURL(repository string) (string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return "", errors.New("更新仓库为空")
	}
	if strings.HasPrefix(repository, "http://") || strings.HasPrefix(repository, "https://") {
		parsed, err := url.Parse(repository)
		if err != nil {
			return "", err
		}
		host := strings.ToLower(parsed.Host)
		if strings.Contains(host, "api.github.com") {
			return repository, nil
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) < 2 {
			return "", fmt.Errorf("无法解析仓库地址: %s", repository)
		}
		repo := fmt.Sprintf("%s/%s", parts[0], parts[1])
		return fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo), nil
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repository), nil
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: time.Duration(defaultHTTPTimeoutSeconds) * time.Second}
	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)
	return client
}

func pickAsset(assets []releaseAsset, prefix string) *releaseAsset {
	if len(assets) == 0 {
		return nil
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	bestScore := -1
	var best *releaseAsset
	for _, asset := range assets {
		name := strings.ToLower(strings.TrimSpace(asset.Name))
		if name == "" {
			continue
		}
		if prefix != "" && !strings.Contains(name, prefix) {
			continue
		}
		if !matchPlatform(name) {
			continue
		}
		score := 0
		if strings.Contains(name, runtime.GOOS+"_"+runtime.GOARCH) {
			score += 4
		}
		if strings.Contains(name, runtime.GOOS+"-"+runtime.GOARCH) {
			score += 3
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			score += 2
		}
		if strings.HasSuffix(name, ".zip") {
			score += 2
		}
		if score > bestScore {
			bestScore = score
			copy := asset
			best = &copy
		}
	}
	return best
}

func matchPlatform(name string) bool {
	name = strings.ToLower(name)
	osKeys := []string{runtime.GOOS}
	if runtime.GOOS == "darwin" {
		osKeys = append(osKeys, "macos")
	}
	archKeys := []string{runtime.GOARCH}
	if runtime.GOARCH == "amd64" {
		archKeys = append(archKeys, "x64")
	}
	if runtime.GOARCH == "arm64" {
		archKeys = append(archKeys, "aarch64")
	}
	hasOS := false
	for _, key := range osKeys {
		if strings.Contains(name, key) {
			hasOS = true
			break
		}
	}
	if !hasOS {
		return false
	}
	for _, key := range archKeys {
		if strings.Contains(name, key) {
			return true
		}
	}
	return false
}

func applyUpdate(ctx context.Context, snapshot configSnapshot, asset *releaseAsset) error {
	if asset == nil {
		return errors.New("更新包为空")
	}
	if asset.BrowserDownloadURL == "" {
		return errors.New("更新包下载地址为空")
	}
	if snapshot.WorkDir == "" {
		return errors.New("更新工作目录为空")
	}
	if err := os.MkdirAll(snapshot.WorkDir, 0o755); err != nil {
		return fmt.Errorf("创建更新目录失败: %w", err)
	}

	workspaceDir := filepath.Join(snapshot.WorkDir, "workspace")
	backupDir := filepath.Join(snapshot.WorkDir, "previous")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("创建更新缓存目录失败: %w", err)
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("创建更新备份目录失败: %w", err)
	}

	packagePath := filepath.Join(workspaceDir, fmt.Sprintf("pkg-%d-%s", time.Now().Unix(), sanitizeFilename(asset.Name)))
	stageDir := filepath.Join(workspaceDir, fmt.Sprintf("stage-%d", time.Now().Unix()))

	client := newHTTPClient(snapshot.ProxyURL)
	if err := downloadAsset(ctx, client, asset.BrowserDownloadURL, packagePath); err != nil {
		return err
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("创建解压目录失败: %w", err)
	}
	if err := extractArchive(packagePath, stageDir); err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前可执行文件失败: %w", err)
	}
	execDir := filepath.Dir(execPath)
	execName := snapshot.ExecutableName
	if execName == "" {
		execName = filepath.Base(execPath)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(execName), ".exe") {
		execName += ".exe"
	}
	targetPath := filepath.Join(execDir, execName)

	newBinaryPath, err := findExecutable(stageDir, execName)
	if err != nil {
		return err
	}
	if err := replaceBinary(newBinaryPath, targetPath, backupDir); err != nil {
		return err
	}
	return nil
}

func downloadAsset(ctx context.Context, client *http.Client, url string, dest string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", selfUpdateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载更新包失败: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("self-update: failed to close download body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("下载更新包失败: status=%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	reader := io.LimitReader(resp.Body, maxSelfUpdateDownloadSize)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("准备下载目录失败: %w", err)
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建更新包失败: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	if _, err := io.Copy(file, reader); err != nil {
		return fmt.Errorf("写入更新包失败: %w", err)
	}
	return nil
}

func extractArchive(path string, dest string) error {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(path, dest)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(path, dest)
	default:
		return fmt.Errorf("不支持的更新包格式: %s", filepath.Base(path))
	}
}

func extractZip(path, dest string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("解压更新包失败: %w", err)
	}
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			log.WithError(errClose).Debug("self-update: failed to close zip reader")
		}
	}()
	for _, file := range reader.File {
		rel := filepath.Clean(file.Name)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dest, rel)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("解压更新包失败: %w", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("解压更新包失败: %w", err)
		}
		reader, errOpen := file.Open()
		if errOpen != nil {
			return fmt.Errorf("解压更新包失败: %w", errOpen)
		}
		out, errCreate := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.Mode())
		if errCreate != nil {
			_ = reader.Close()
			return fmt.Errorf("解压更新包失败: %w", errCreate)
		}
		if _, errCopy := io.Copy(out, reader); errCopy != nil {
			_ = out.Close()
			_ = reader.Close()
			return fmt.Errorf("解压更新包失败: %w", errCopy)
		}
		_ = out.Close()
		_ = reader.Close()
	}
	return nil
}

func extractTarGz(path, dest string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("解压更新包失败: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("解压更新包失败: %w", err)
	}
	defer func() {
		_ = gzr.Close()
	}()
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("解压更新包失败: %w", err)
		}
		rel := filepath.Clean(header.Name)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dest, rel)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("解压更新包失败: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("解压更新包失败: %w", err)
			}
			out, errCreate := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if errCreate != nil {
				return fmt.Errorf("解压更新包失败: %w", errCreate)
			}
			if _, errCopy := io.Copy(out, tr); errCopy != nil {
				_ = out.Close()
				return fmt.Errorf("解压更新包失败: %w", errCopy)
			}
			_ = out.Close()
		}
	}
	return nil
}

func findExecutable(root string, name string) (string, error) {
	if root == "" {
		return "", errors.New("解压目录为空")
	}
	if name == "" {
		return "", errors.New("可执行文件名为空")
	}
	match := strings.ToLower(name)
	var candidate string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Base(path)) == match {
			candidate = path
			return errSelfUpdateFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSelfUpdateFound) {
		return "", fmt.Errorf("查找可执行文件失败: %w", err)
	}
	if candidate == "" {
		return "", fmt.Errorf("未找到可执行文件: %s", name)
	}
	return candidate, nil
}

func replaceBinary(newBinary, targetPath, backupDir string) error {
	if newBinary == "" || targetPath == "" {
		return errors.New("可执行文件路径为空")
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	tempPath := targetPath + ".new"
	if err := copyFile(newBinary, tempPath, 0o755); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s.%d", filepath.Base(targetPath), time.Now().Unix()))
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.Rename(targetPath, backupPath); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("备份旧版本失败: %w", err)
		}
		if err := os.Rename(tempPath, targetPath); err != nil {
			_ = os.Rename(backupPath, targetPath)
			_ = os.Remove(tempPath)
			return fmt.Errorf("替换可执行文件失败: %w", err)
		}
	} else if os.IsNotExist(err) {
		if err := os.Rename(tempPath, targetPath); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("写入新版本失败: %w", err)
		}
	} else {
		_ = os.Remove(tempPath)
		return fmt.Errorf("检查旧版本失败: %w", err)
	}
	return nil
}

func copyFile(src, dest string, mode os.FileMode) error {
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读取更新文件失败: %w", err)
	}
	defer func() {
		_ = input.Close()
	}()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("准备更新目录失败: %w", err)
	}
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("写入更新文件失败: %w", err)
	}
	defer func() {
		_ = output.Close()
	}()
	if _, err := io.Copy(output, input); err != nil {
		return fmt.Errorf("写入更新文件失败: %w", err)
	}
	return nil
}

func sanitizeFilename(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "package"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_")
	return replacer.Replace(trimmed)
}

func scheduleRestart(delaySeconds int) {
	if delaySeconds <= 0 {
		delaySeconds = config.DefaultSelfUpdateRestartDelaySeconds
	}
	delay := time.Duration(delaySeconds) * time.Second
	go func() {
		time.Sleep(delay)
		log.Warn("self-update: exiting for restart")
		os.Exit(0)
	}()
}

func isRunningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	for _, path := range []string{"/proc/1/cgroup", "/proc/self/cgroup"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "docker") || strings.Contains(lower, "containerd") || strings.Contains(lower, "kubepods") {
			return true
		}
	}
	return false
}

func hasNewerVersion(currentVersion, latestVersion string) bool {
	currentTuple := parseVersionTuple(currentVersion)
	latestTuple := parseVersionTuple(latestVersion)
	if currentTuple != nil && latestTuple != nil {
		width := len(currentTuple)
		if len(latestTuple) > width {
			width = len(latestTuple)
		}
		currentTuple = padVersionTuple(currentTuple, width)
		latestTuple = padVersionTuple(latestTuple, width)
		for i := 0; i < width; i++ {
			if latestTuple[i] > currentTuple[i] {
				return true
			}
			if latestTuple[i] < currentTuple[i] {
				return false
			}
		}
		return false
	}
	return normalizeVersion(latestVersion) != normalizeVersion(currentVersion)
}

func parseVersionTuple(value string) []int {
	normalized := normalizeVersion(value)
	if normalized == "" {
		return nil
	}
	if normalized[0] < '0' || normalized[0] > '9' {
		return nil
	}
	parts := strings.FieldsFunc(normalized, func(r rune) bool {
		return r < '0' || r > '9'
	})
	if len(parts) == 0 {
		return nil
	}
	result := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		var num int
		for _, ch := range part {
			num = num*10 + int(ch-'0')
		}
		result = append(result, num)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func padVersionTuple(tuple []int, width int) []int {
	if len(tuple) >= width {
		return tuple
	}
	out := make([]int, width)
	copy(out, tuple)
	return out
}

func normalizeVersion(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	normalized = strings.TrimPrefix(normalized, "v")
	return normalized
}
