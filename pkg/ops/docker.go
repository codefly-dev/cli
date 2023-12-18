package ops

//
//imports (
//	"archive/tar"
//	"bufio"
//	"bytes"
//	"context"
//	"encoding/json"
//	"fmt"
//	"github.com/docker/docker/api/types"
//	"github.com/docker/docker/client"
//	"github.com/codefly-dev/cli/pkg/platform/agents"
//	"io"
//	"os"
//	"path/filepath"
//	"strings"
//)
//
//type Docker struct {
//	cli    *client.Client
//	logger *agents.AgentLogger
//}
//
//func NewDocker(logger *agents.AgentLogger) (*Docker, error) {
//	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
//	if err != nil {
//		return nil, fmt.Errorf("cannot create docker client: %w", err)
//	}
//	return &Docker{
//		cli:    cli,
//		logger: logger,
//	}, nil
//}
//
//type DockerBuildConfiguration struct {
//	Image        string
//	Dockerfile   string
//	Dependencies []string
//}
//
//func (d *Docker) BuildImage(ctx context.Context, context DockerBuildConfiguration) error {
//	// Add a buffer to write our archive to.
//	buf := new(bytes.Buffer)
//
//	// Add a new tar archive.
//	tw := tar.NewWriter(buf)
//
//	// Map dependencies
//	for _, dep := range context.Dependencies {
//		info, err := os.Stat(dep)
//		if err != nil {
//			return fmt.Errorf("cannot stat dependency: %w", err)
//		}
//		if info.IsDir() {
//			if err := addDirToTarWriter(dep, tw); err != nil {
//				return fmt.Errorf("cannot add directory to tar archive: %w", err)
//			}
//			continue
//		}
//		// Map single file
//		if err := addFileToTarWriter(dep, tw); err != nil {
//			return fmt.Errorf("cannot add file to tar archive: %w", err)
//		}
//
//	}
//
//	// Map nginx.conf to tar archive
//	tarHeader := &tar.Header{
//		ProjectName: "nginx.conf",
//		Size: int64(len(context.Dockerfile)),
//	}
//	if err := tw.WriteHeader(tarHeader); err != nil {
//		return fmt.Errorf("cannot write tar header: %w", err)
//	}
//	if _, err := tw.Write([]byte(context.Dockerfile)); err != nil {
//		return fmt.Errorf("cannot write tar body: %w", err)
//	}
//	// Close tar archive
//	if err := tw.Close(); err != nil {
//		return fmt.Errorf("cannot close tar archive: %w", err)
//	}
//
//	// Build the image
//	resp, err := d.cli.ImageBuild(
//		context.Background(),
//		buf,
//		types.ImageBuildOptions{
//			Tags:       []string{context.Image},
//			Dockerfile: "nginx.conf",
//		},
//	)
//	if err != nil {
//		return fmt.Errorf("cannot build image: %w", err)
//	}
//	defer resp.Body.Close()
//
//	// Read the logs
//	scanner := bufio.NewScanner(resp.Body)
//	for scanner.Scan() {
//		var output DockerOutput
//		if err := json.Unmarshal(scanner.Bytes(), &output); err != nil {
//			d.logger.Errorf("Errorf decoding JSON:", err)
//			continue
//		}
//
//		if output.Stream != "" {
//			d.logger.Info(strings.TrimSpace(output.Stream))
//		}
//
//		if output.Errorf != "" {
//			d.logger.Errorf("BUILD", fmt.Errorf("%s: %s", output.Errorf, output.ErrorDetail.Message))
//		}
//	}
//
//	if err := scanner.Err(); err != nil {
//		return fmt.Errorf("error reading output: %v", err)
//	}
//	return nil
//}
//
//func (d *Docker) TagImage(s string) (string, error) {
//	// Prefix with codefly/
//	target := fmt.Sprintf("codefly/%s", s)
//	err := d.cli.ImageTag(context.Background(), s, target)
//	if err != nil {
//		return "", fmt.Errorf("cannot tag image: %w", err)
//	}
//	return target, nil
//}
//
//type DockerOutput struct {
//	Stream      string `json:"stream"`
//	ErrorDetail struct {
//		Code    int    `json:"code"`
//		Message string `json:"message"`
//	} `json:"errorDetail"`
//	Errorf string `json:"error"`
//}
//
//// Adds a single file to the tar writer
//func addFileToTarWriter(filePath string, tw *tar.Writer) error {
//	// Open the file which needs to be added to the tar
//	file, err := os.Open(filePath)
//	if err != nil {
//		return err
//	}
//	defer file.Close()
//
//	// Get the file stats
//	fileInfo, err := file.Stat()
//	if err != nil {
//		return err
//	}
//
//	// Add a tar header from the fileInfo data
//	header, err := tar.FileInfoHeader(fileInfo, fileInfo.ProjectName())
//	if err != nil {
//		return err
//	}
//
//	// Update the name to correctly reflect the desired destination when untaring
//	header.ProjectName = strings.TrimPrefix(filePath, string(os.PathSeparator))
//
//	// Write the header into the tar archive
//	if err := tw.WriteHeader(header); err != nil {
//		return err
//	}
//
//	// Write the file data to the tar element
//	if _, err := io.Copy(tw, file); err != nil {
//		return err
//	}
//
//	return nil
//}
//
//func addDirToTarWriter(dirPath string, tw *tar.Writer) error {
//	return filepath.Walk(dirPath, func(file string, fi os.FileInfo, err error) error {
//		if err != nil {
//			return err
//		}
//
//		// create a new dir/file header
//		header, err := tar.FileInfoHeader(fi, fi.ProjectName())
//		if err != nil {
//			return err
//		}
//
//		// update the name to correctly reflect the desired destination when untaring
//		header.ProjectName = strings.TrimPrefix(strings.Replace(file, dirPath, "", -1), string(filepath.Separator))
//
//		// write the header
//		if err := tw.WriteHeader(header); err != nil {
//			return err
//		}
//
//		// if it's not a dir, write file content
//		if !fi.IsDir() {
//			data, err := os.Open(file)
//			if err != nil {
//				return err
//			}
//			if _, err := io.Copy(tw, data); err != nil {
//				return err
//			}
//		}
//		return nil
//	})
//}
