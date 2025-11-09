package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/manifoldco/promptui"
	// Assuming this dependency is available based on original code
)

// Net implements the Runtime interface for .NET projects.
type Net struct {
	Log *slog.Logger
}

func (d *Net) Name() RuntimeName {
	return RuntimeNameNet
}

// Match checks for common .NET project files (.csproj, .fsproj, .vbproj).
func (d *Net) Match(path string) bool {
	// 检查常见的 .NET 项目文件
	projectFilePatterns := []string{"*.csproj", "*.fsproj", "*.vbproj"}
	for _, pattern := range projectFilePatterns {
		// 使用 Glob 查找匹配模式的文件
		matches, err := filepath.Glob(filepath.Join(path, pattern))
		if err == nil && len(matches) > 0 {
			d.Log.Info("Detected .NET project")
			return true
		}
	}
	d.Log.Debug(".NET project not detected")
	return false
}

// GenerateDockerfile generates a multi-stage Dockerfile for a .NET project.
func (d *Net) GenerateDockerfile(path string, data ...map[string]string) ([]byte, error) {
	// 1. 查找 .NET SDK 版本
	version, err := findNetVersion(path, d.Log)
	if err != nil {
		return nil, err
	}

	// 2. 查找主项目文件 (用于 dotnet publish)
	projectFile, err := findProjectFile(path)
	if err != nil {
		// 如果未找到，我们仍然可以继续，但会在 Dockerfile 中使用通配符或警告
		d.Log.Warn(fmt.Sprintf("Could not locate a single main project file: %v. Using '*' as placeholder.", err))
		projectFile = "" // 在 Dockerfile 中可能使用 . 或通配符
	}

	d.Log.Info(
		fmt.Sprintf(`Detected .NET defaults 
  .NET SDK Version: %s
  Project File    : %s
`, *version, projectFile),
	)

	// 3. 准备模板数据
	templateData := map[string]string{
		"Version":     *version,
		"ProjectFile": projectFile,
		"PublishDir":  "/app/publish",
		"Port":        getPort(),
	}
	if len(data) > 0 {
		maps.Copy(templateData, data[0])
	}

	// 4. 解析并执行模板
	tmpl, err := template.New("Dockerfile").Parse(netTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse .NET template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Option("missingkey=zero").Execute(&buf, templateData); err != nil {
		return nil, errors.New("failed to execute .NET template")
	}

	return buf.Bytes(), nil
}

func getPort() string {
	validate := func(input string) error {
		if input == "" {
			return errors.New("invalid subscription id")
		}
		return nil
	}

	prompt := promptui.Prompt{
		Label:    "Enter Port",
		Validate: validate,
	}

	result, err := prompt.Run()

	if err != nil {
		return err.Error()
	}

	return result
}

// findProjectFile attempts to locate the primary .NET project file.
func findProjectFile(path string) (string, error) {
	patterns := []string{"*.csproj", "*.fsproj", "*.vbproj"}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(path, pattern))
		if err == nil && len(matches) > 0 {
			// 找到一个就返回，通常在单项目仓库中这是正确的选择
			fileName := filepath.Base(matches[0])
			// 移除文件后缀
			return strings.TrimSuffix(fileName, filepath.Ext(fileName)), nil
		}
	}
	return "", errors.New("no .NET project file found")
}

// findNetVersion determines the .NET SDK version from project metadata.
func findNetVersion(path string, log *slog.Logger) (*string, error) {
	// 1. 检查 global.json 文件
	globalJsonPath := filepath.Join(path, "global.json")
	if _, err := os.Stat(globalJsonPath); err == nil {
		var globalJSON struct {
			SDK struct {
				Version string `json:"version"`
			} `json:"sdk"`
		}
		f, err := os.Open(globalJsonPath)
		if err == nil {
			defer f.Close()
			if json.NewDecoder(f).Decode(&globalJSON) == nil && globalJSON.SDK.Version != "" {
				// global.json 提供了完整的语义版本，我们提取 Major.Minor 部分作为 Docker 标签
				if majorMinor := extractMajorMinor(globalJSON.SDK.Version); majorMinor != "" {
					log.Info("Detected .NET SDK version from global.json: " + majorMinor)
					return &majorMinor, nil
				}
			}
		}
	}

	// 2. 检查项目文件中的 TargetFramework
	patterns := []string{"*.csproj", "*.fsproj", "*.vbproj"}
	// 匹配 <TargetFramework>netX.Y</TargetFramework>
	regex := regexp.MustCompile(`<TargetFramework>net([\d\.]+)</TargetFramework>`)

	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(path, pattern))
		for _, match := range matches {
			content, err := os.ReadFile(match)
			if err == nil {
				if submatches := regex.FindStringSubmatch(string(content)); len(submatches) > 1 {
					version := submatches[1] // e.g., "8.0", "7.0"
					log.Info("Detected .NET TargetFramework from " + filepath.Base(match) + ": " + version)
					return &version, nil
				}
			}
		}
	}

	// 3. 默认值
	defaultVersion := "8.0" // 使用最新的 LTS 版本作为默认值
	log.Info("No .NET version detected. Using default LTS: " + defaultVersion)
	return &defaultVersion, nil
}

// extractMajorMinor 从语义版本中提取 Major.Minor (例如: "8.0.100" -> "8.0", "7.0.5" -> "7.0")
func extractMajorMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// netTemplate 是一个多阶段的 .NET Dockerfile 模板。
// 它使用 'sdk' 镜像进行构建和发布，然后切换到更小的 'aspnet' 镜像进行运行。
var netTemplate = strings.TrimSpace(`
# Multi-stage Dockerfile for .NET Application

# -----------------
# 1. Build Stage
# -----------------
# ARG NET_VERSION: .NET SDK 版本，例如 8.0
ARG NET_VERSION={{.Version}}
ARG PORT={{.Port}}
# 使用 SDK 镜像进行构建和发布
FROM mcr.microsoft.com/dotnet/sdk:${NET_VERSION} AS build
WORKDIR /src

# 复制项目文件并恢复依赖
# 假设主项目文件在根目录
COPY *.csproj *.fsproj *.vbproj ./
# 运行 dotnet restore，恢复依赖
RUN dotnet restore

# 复制剩余的源代码并构建
COPY . .
# 发布应用到 /app/publish 目录
# {{.ProjectFile}} 变量应包含主项目文件名，如 MyWebApp.csproj
RUN dotnet publish "{{.ProjectFile}}" -c Release -o /app/publish --no-restore

# -----------------
# 2. Runtime Stage
# -----------------
# 使用更小、更安全的 ASP.NET Runtime 镜像
FROM mcr.microsoft.com/dotnet/aspnet:${NET_VERSION} AS final
WORKDIR /app

# 从 build 阶段复制发布的输出
COPY --from=build /app/publish .

# 配置环境变量和端口
ENV ASPNETCORE_URLS=http://+:${PORT:-8080}
ENV DOTNET_RUNNING_IN_CONTAINER=true
EXPOSE {{.Port}}

# 容器启动入口点
# 应用程序的 DLL 文件名通常与项目文件同名，例如 MyWebApp.csproj -> MyWebApp.dll
ENTRYPOINT ["dotnet", "{{.ProjectFile}}.dll"]
`)
