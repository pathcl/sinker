package commands

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	promv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/ghodss/yaml"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DockerImage struct {
	Host       string
	Name       string
	Repository string
	Version    string
}

func (d DockerImage) String() string {
	var output string
	if d.Host != "" {
		output = d.Host + "/"
	}

	output += d.Repository + ":" + d.Version

	return output
}

// NewListCommand creates a new list command
func NewListCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "list",
		Short: "List the images found in the repository",
		Args:  cobra.ExactArgs(1),

		RunE: func(cmd *cobra.Command, args []string) error {
			if err := viper.BindPFlag("output", cmd.Flags().Lookup("output")); err != nil {
				return fmt.Errorf("bind flag: %w", err)
			}

			if err := runListCommand(args); err != nil {
				return fmt.Errorf("list: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringP("output", "o", "", fmt.Sprintf("output path for the image list"))

	return &cmd
}

func runListCommand(args []string) error {
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}

	listPath := filepath.Join(workingDir, args[0])
	images, err := GetImagesInPath(listPath)
	if err != nil {
		return fmt.Errorf("get images from path: %w", err)
	}

	if viper.GetString("output") != "" {
		outputFile := filepath.Join(workingDir, viper.GetString("output"))
		writeListToFile(images, outputFile)
	} else {
		for _, image := range images {
			fmt.Println(image)
		}
	}

	return nil
}

func GetImagesInPath(path string) ([]DockerImage, error) {
	files, err := getYamlFiles(path)
	if err != nil {
		return nil, fmt.Errorf("get yaml files: %w", err)
	}

	yamlFiles, err := splitYamlFiles(files)
	if err != nil {
		return nil, fmt.Errorf("split yaml files: %w", err)
	}

	type BaseSpec struct {
		Template corev1.PodTemplateSpec `json:"template" protobuf:"bytes,3,opt,name=template"`
	}

	type BaseType struct {
		Spec BaseSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	}

	var imageList []string
	for _, yamlFile := range yamlFiles {
		var contents BaseType

		var typeMeta metav1.TypeMeta
		if err := yaml.Unmarshal(yamlFile, &typeMeta); err != nil {
			continue
		}

		if typeMeta.Kind == "Prometheus" {
			var prometheus promv1.Prometheus
			if err := yaml.Unmarshal(yamlFile, &prometheus); err != nil {
				return nil, fmt.Errorf("unmarshal prometheus: %w", err)
			}

			prometheusImage := prometheus.Spec.BaseImage + ":" + prometheus.Spec.Version
			imageList = append(imageList, prometheusImage)
			continue
		}

		if typeMeta.Kind == "Alertmanager" {
			var alertmanager promv1.Alertmanager
			if err := yaml.Unmarshal(yamlFile, &alertmanager); err != nil {
				return nil, fmt.Errorf("unmarshal alertmanager: %w", err)
			}

			alertmanagerImage := alertmanager.Spec.BaseImage + ":" + alertmanager.Spec.Version
			imageList = append(imageList, alertmanagerImage)
			continue
		}

		if err := yaml.Unmarshal(yamlFile, &contents); err != nil {
			continue
		}

		if len(contents.Spec.Template.Spec.InitContainers) > 0 {
			imageList = append(imageList, getImagesFromContainers(contents.Spec.Template.Spec.InitContainers)...)
		}

		if len(contents.Spec.Template.Spec.Containers) > 0 {
			imageList = append(imageList, getImagesFromContainers(contents.Spec.Template.Spec.Containers)...)
		}
	}

	dedupedImageList := dedupeImages(imageList)

	marshaledImages := marshalImages(dedupedImageList)

	return marshaledImages, nil
}

func marshalImages(images []string) []DockerImage {
	var marshaledImages []DockerImage
	for _, image := range images {
		imageTokens := strings.Split(image, ":")
		imagePaths := strings.Split(imageTokens[0], "/")
		imageName := imagePaths[len(imagePaths)-1]

		var imageHost string
		var imageRepository string
		if strings.Contains(imagePaths[0], ".io") {
			imageHost = imagePaths[0]
		} else {
			imageHost = ""
		}

		if imageHost != "" {
			imageRepository = strings.TrimPrefix(imageTokens[0], imageHost+"/")
		} else {
			imageRepository = imageTokens[0]
		}

		dockerImage := DockerImage{
			Host:       imageHost,
			Repository: imageRepository,
			Name:       imageName,
			Version:    imageTokens[1],
		}

		marshaledImages = append(marshaledImages, dockerImage)
	}

	return marshaledImages
}

func writeListToFile(images []DockerImage, outputFile string) error {
	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer f.Close()

	for _, value := range images {
		fmt.Fprintln(f, value)
	}

	return nil
}

func getImagesFromContainers(containers []corev1.Container) []string {
	var images []string
	for _, container := range containers {
		images = append(images, container.Image)

		argImages := getImagesFromContainerArgs(container.Args)

		images = append(images, argImages...)
	}

	return images
}

func getImagesFromContainerArgs(args []string) []string {
	var images []string
	for _, arg := range args {
		if !strings.Contains(arg, ":") || strings.Contains(arg, "=:") {
			continue
		}

		argTokens := strings.Split(arg, "=")
		images = append(images, argTokens[1])
	}

	return images
}

func getYamlFiles(path string) ([]string, error) {
	var files []string
	err := filepath.Walk(path, func(currentFilePath string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walk path: %w", err)
		}

		if fileInfo.IsDir() && fileInfo.Name() == ".git" {
			return filepath.SkipDir
		}

		if fileInfo.IsDir() {
			return nil
		}

		if filepath.Ext(currentFilePath) != ".yaml" && filepath.Ext(currentFilePath) != ".yml" {
			return nil
		}

		files = append(files, currentFilePath)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return files, nil
}

func splitYamlFiles(files []string) ([][]byte, error) {
	var yamlFiles [][]byte
	for _, file := range files {
		fileContent, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("open file: %w", err)
		}

		individualYamlFiles := doSplit(fileContent)

		for _, yamlFile := range individualYamlFiles {
			yamlFiles = append(yamlFiles, yamlFile)
		}
	}

	return yamlFiles, nil
}

func contains(images []string, image string) bool {
	for _, currentImage := range images {
		if strings.EqualFold(currentImage, image) {
			return true
		}
	}

	return false
}

func dedupeImages(images []string) []string {
	var dedupedImageList []string
	for _, image := range images {
		if !contains(dedupedImageList, image) {
			dedupedImageList = append(dedupedImageList, image)
		}
	}

	return dedupedImageList
}

func doSplit(data []byte) [][]byte {
	linebreak := "\n"
	windowsLineEnding := bytes.Contains(data, []byte("\r\n"))
	if windowsLineEnding && runtime.GOOS == "windows" {
		linebreak = "\r\n"
	}

	return bytes.Split(data, []byte(linebreak+"---"+linebreak))
}