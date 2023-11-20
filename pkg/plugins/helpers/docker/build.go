package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/shared"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type Env struct {
	Key   string
	Value string
}

type BuilderConfiguration struct {
	Root       string
	Image      string
	Dockerfile string
	Tag        string
}

type Builder struct {
	logger shared.BaseLogger
	BuilderConfiguration
}

func NewBuilder(cfg BuilderConfiguration) (*Builder, error) {
	logger := shared.NewLogger("docker.NewBuilder")
	return &Builder{BuilderConfiguration: cfg, logger: logger}, nil
}

func (builder *Builder) WithLogger(logger shared.BaseLogger) {
	builder.logger = logger
}

type BuilderOutput struct{}

func (builder *Builder) Build() (*BuilderOutput, error) {
	builder.logger.Debugf("Building image %s from %s", builder.Image, builder.Root)
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, builder.logger.Wrapf(err, "cannot create docker client")
	}

	buildContextBuffer, err := createTarArchive(builder.logger, builder.Root)
	if err != nil {
		return nil, builder.logger.Wrapf(err, "cannot create tar archive")
	}
	buildContext := buildContextBuffer.Bytes()

	// Build the Docker image
	tag := "latest"
	if builder.Tag != "" {
		tag = builder.Tag
	}
	imageBuildResponse, err := cli.ImageBuild(
		ctx,
		bytes.NewReader(buildContext),
		types.ImageBuildOptions{
			Dockerfile: builder.Dockerfile,
			Tags:       []string{fmt.Sprintf("%s:%s", builder.Image, tag)},
			Remove:     true,
		},
	)
	if err != nil {
		return nil, builder.logger.Wrapf(err, "cannot build image")
	}

	// Respond the build output
	scanner := bufio.NewScanner(imageBuildResponse.Body)
	var buildOutput struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
		Stream string `json:"stream"`
	}
	for scanner.Scan() {
		line := scanner.Bytes()

		if err := json.Unmarshal(line, &buildOutput); err != nil {
			builder.logger.Debugf("Error during build from parsing <%s>: %v", string(line), err)
			continue
		}

		if buildOutput.Error != nil {
			builder.logger.Debugf("Error during build: %s\n", buildOutput.Error.Message)
		} else {
			builder.logger.Debugf(buildOutput.Stream)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading build output: %v\n", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			builder.logger.TODO("Error closing build response body: %v", err)
		}
	}(imageBuildResponse.Body)
	return nil, nil
}

// createTarArchive creates a tar archive from the provided directory and returns it as a bytes buffer.
func createTarArchive(logger shared.BaseLogger, srcDir string) (*bytes.Buffer, error) {
	// Add a buffer to write our archive to.
	buf := new(bytes.Buffer)

	// Add a new tar archive.
	tw := tar.NewWriter(buf)

	// Walk through each file/folder in the path and add it to the tar archive.
	err := filepath.Walk(srcDir, func(file string, fi os.FileInfo, err error) error {
		// Return any error.
		if err != nil {
			return err
		}

		// Add a new dir/file header.
		header, err := tar.FileInfoHeader(fi, file)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, file)
		if err != nil {
			return err
		}

		header.Name = filepath.Join(rel)

		// Write the header.
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		// If it's not a directory, write the file content.
		if !fi.Mode().IsDir() {
			data, err := os.Open(file)
			if err != nil {
				return err
			}
			defer data.Close()

			if _, err := io.Copy(tw, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Make sure to check the error on Close.
	if err := tw.Close(); err != nil {
		return nil, err
	}

	return buf, nil
}

//func printTarContents(logger *plugins.PluginLogger, tarBuffer *bytes.Buffer) {
//	tarReader := tar.NewReader(tarBuffer)
//
//	logger.Debugf("Contents of the tar archive:")
//	for {
//		header, err := tarReader.Next()
//		if err == io.EOF {
//			break // End of archive
//		}
//		if err != nil {
//			logger.Debugf("got error: %v", err)
//			break
//		}
//
//		logger.Debugf("FILE: %v", header.Name)
//	}
//}
//
//func printFileContentFromTar(logger *plugins.PluginLogger, tarBuffer *bytes.Buffer, filename string) error {
//	tarReader := tar.NewReader(tarBuffer)
//
//	for {
//		header, err := tarReader.Next()
//		if err == io.EOF {
//			break // End of archive
//		}
//		if err != nil {
//			return fmt.Errorf("error reading tar header: %v", err)
//		}
//
//		if header.Name == filename {
//			content, err := ioutil.ReadAll(tarReader)
//			if err != nil {
//				return fmt.Errorf("error reading file content: %v", err)
//			}
//			logger.Debugf("Content of %s:\n%s\n", filename, content)
//			return nil
//		}
//	}
//
//	logger.Debugf("File %s not found in tar archive\n", filename)
//	return nil
//}
