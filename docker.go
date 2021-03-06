package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/blang/semver"
	"github.com/coveo/gotemplate/utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/fatih/color"
	"github.com/gruntwork-io/terragrunt/util"
)

const (
	minimumDockerVersion = "1.25"
	tgfImageVersion      = "TGF_IMAGE_VERSION"
	dockerSocketFile     = "/var/run/docker.sock"
)

func callDocker(withDockerMount bool, args ...string) int {
	command := append([]string{config.EntryPoint}, args...)

	// Change the default log level for terragrunt
	const logLevelArg = "--terragrunt-logging-level"
	if !util.ListContainsElement(command, logLevelArg) && filepath.Base(config.EntryPoint) == "terragrunt" {
		if config.LogLevel == "6" || strings.ToLower(config.LogLevel) == "full" {
			config.LogLevel = "debug"
			config.Environment["TF_LOG"] = "DEBUG"
			config.Environment["TERRAGRUNT_DEBUG"] = "1"
		}

		// The log level option should not be supplied if there is no actual command
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				command = append(command, []string{logLevelArg, config.LogLevel}...)
				break
			}
		}
	}

	if flushCache && filepath.Base(config.EntryPoint) == "terragrunt" {
		command = append(command, "--terragrunt-source-update")
	}

	imageName := getImage()

	if getImageName {
		Println(imageName)
		return 0
	}

	cwd := filepath.ToSlash(must(filepath.EvalSymlinks(must(os.Getwd()).(string))).(string))
	currentDrive := fmt.Sprintf("%s/", filepath.VolumeName(cwd))
	sourceFolder := filepath.ToSlash(filepath.Join("/", mountPoint, strings.TrimPrefix(cwd, currentDrive)))
	rootFolder := strings.Split(strings.TrimPrefix(cwd, currentDrive), "/")[0]

	dockerArgs := []string{
		"run", "-it",
		"-v", fmt.Sprintf("%s%s:%s", convertDrive(currentDrive), rootFolder, filepath.ToSlash(filepath.Join("/", mountPoint, rootFolder))),
		"-w", sourceFolder,
	}

	if withDockerMount {
		withDockerMountArgs := []string{"-v", fmt.Sprintf(dockerSocketMountPattern, dockerSocketFile), "--group-add", getDockerGroup()}
		dockerArgs = append(dockerArgs, withDockerMountArgs...)
	}

	if !noHome {
		currentUser := must(user.Current()).(*user.User)
		home := filepath.ToSlash(currentUser.HomeDir)
		homeWithoutVolume := strings.TrimPrefix(home, filepath.VolumeName(home))

		dockerArgs = append(dockerArgs, []string{
			"-v", fmt.Sprintf("%v:%v", convertDrive(home), homeWithoutVolume),
			"-e", fmt.Sprintf("HOME=%v", homeWithoutVolume),
		}...)

		dockerArgs = append(dockerArgs, config.DockerOptions...)
	}

	if !noTemp {
		temp := filepath.ToSlash(filepath.Join(must(filepath.EvalSymlinks(os.TempDir())).(string), "tgf-cache"))
		tempDrive := fmt.Sprintf("%s/", filepath.VolumeName(temp))
		tempFolder := strings.TrimPrefix(temp, tempDrive)
		if runtime.GOOS == "windows" {
			os.Mkdir(temp, 0755)
		}
		dockerArgs = append(dockerArgs, "-v", fmt.Sprintf("%s%s:/var/tgf", convertDrive(tempDrive), tempFolder))
		config.Environment["TERRAGRUNT_CACHE"] = "/var/tgf"
	}

	config.Environment["TGF_COMMAND"] = config.EntryPoint
	config.Environment["TGF_VERSION"] = version
	config.Environment["TGF_ARGS"] = strings.Join(os.Args, " ")
	config.Environment["TGF_LAUNCH_FOLDER"] = sourceFolder
	config.Environment["TGF_IMAGE_NAME"] = imageName // sha256 of image

	if !strings.Contains(config.Image, "coveo/tgf") { // the tgf image injects its own image info
		config.Environment["TGF_IMAGE"] = config.Image
		if config.ImageVersion != nil {
			config.Environment[tgfImageVersion] = *config.ImageVersion
			if version, err := semver.Make(*config.ImageVersion); err == nil {
				config.Environment["TGF_IMAGE_MAJ_MIN"] = fmt.Sprintf("%d.%d", version.Major, version.Minor)
			}
		}
		if config.ImageTag != nil {
			config.Environment["TGF_IMAGE_TAG"] = *config.ImageTag
		}
	}

	for key, val := range config.Environment {
		os.Setenv(key, val)
		debugPrint("export %v=%v", key, val)
	}

	for _, do := range dockerOptions {
		dockerArgs = append(dockerArgs, strings.Split(do, " ")...)
	}

	if !util.ListContainsElement(dockerArgs, "--name") {
		// We do not remove the image after execution if a name has been provided
		dockerArgs = append(dockerArgs, "--rm")
	}

	dockerArgs = append(dockerArgs, getEnviron(!noHome)...)
	dockerArgs = append(dockerArgs, imageName)
	dockerArgs = append(dockerArgs, command...)
	dockerCmd := exec.Command("docker", dockerArgs...)
	dockerCmd.Stdin, dockerCmd.Stdout = os.Stdin, os.Stdout
	var stderr bytes.Buffer
	dockerCmd.Stderr = &stderr

	if len(config.Environment) > 0 {
		debugPrint("")
	}
	debugPrint("%s\n", strings.Join(dockerCmd.Args, " "))

	if err := runCommands(config.runBeforeCommands); err != nil {
		return -1
	}
	if err := dockerCmd.Run(); err != nil {
		if stderr.Len() > 0 {
			ErrPrintf(errorString(stderr.String()))
			ErrPrintf("\n%s %s\n", dockerCmd.Args[0], strings.Join(dockerArgs, " "))

			if runtime.GOOS == "windows" {
				ErrPrintln(windowsMessage)
			}
		}
	}
	if err := runCommands(config.runAfterCommands); err != nil {
		ErrPrintf(errorString("%v", err))
	}

	return dockerCmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
}

func debugPrint(format string, args ...interface{}) {
	if debugMode {
		ErrPrintf(color.HiBlackString(format+"\n", args...))
	}
}

func runCommands(commands []string) error {
	for _, script := range commands {
		cmd, tempFile, err := utils.GetCommandFromString(script)
		if err != nil {
			return err
		}
		if tempFile != "" {
			defer func() { os.Remove(tempFile) }()
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

// Returns the image name to use
// If docker-image-build option has been set, an image is dynamically built and the resulting image digest is returned
func getImage() (name string) {
	name = config.GetImageName()
	if !strings.Contains(name, ":") {
		name += ":latest"
	}

	for i, ib := range config.imageBuildConfigs {
		var temp, folder, dockerFile string
		var out *os.File
		if ib.Folder == "" {
			// There is no explicit folder, so we create a temporary folder to store the docker file
			temp = must(ioutil.TempDir("", "tgf-dockerbuild")).(string)
			out = must(os.Create(filepath.Join(temp, "Dockerfile"))).(*os.File)
			folder = temp
		} else {
			if ib.Instructions != "" {
				out = must(ioutil.TempFile(ib.Dir(), "DockerFile")).(*os.File)
				temp = out.Name()
				dockerFile = temp
			}
			folder = ib.Dir()
		}

		if out != nil {
			ib.Instructions = fmt.Sprintf("FROM %s\n%s\n", name, ib.Instructions)
			must(fmt.Fprintf(out, ib.Instructions))
			must(out.Close())
		}

		if temp != "" {
			// A temporary file of folder has been created, we register functions to ensure proper cleanup
			cleanup := func() { os.Remove(temp) }
			defer cleanup()
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-c
				Println("\nRemoving file", dockerFile)
				cleanup()
				panic(errorString("Execution interrupted by user: %v", c))
			}()
		}

		name = name + "-" + ib.GetTag()
		if refresh || getActualImageVersionInternal(name) == "" {
			args := []string{"build", ".", "--quiet", "--force-rm"}
			if i == 0 && refresh {
				args = append(args, "--pull")
			}
			if dockerFile != "" {
				args = append(args, "--file")
				args = append(args, filepath.Base(dockerFile))
			}

			args = append(args, "--tag", name)
			buildCmd := exec.Command("docker", args...)

			debugPrint("%s", strings.Join(buildCmd.Args, " "))
			if ib.Instructions != "" {
				debugPrint("%s", ib.Instructions)
			}
			buildCmd.Stderr = os.Stderr
			buildCmd.Dir = folder
			must(buildCmd.Output())
			prune()
		}
	}

	return
}

func prune(images ...string) {
	cli, ctx := getDockerClient()
	if len(images) > 0 {
		current := fmt.Sprintf(">=%s", GetActualImageVersion())
		for _, image := range images {
			filters := filters.NewArgs()
			filters.Add("reference", image)
			if images, err := cli.ImageList(ctx, types.ImageListOptions{Filters: filters}); err == nil {
				for _, image := range images {
					actual := getActualImageVersionFromImageID(image.ID)
					if actual == "" {
						for _, tag := range image.RepoTags {
							matches, _ := utils.MultiMatch(tag, reImage)
							if version := matches["version"]; version != "" {
								if len(version) > len(actual) {
									actual = version
								}
							}
						}
					}
					upToDate, err := CheckVersionRange(actual, current)
					if err != nil {
						ErrPrintln("Check version for %s vs%s: %v", actual, current, err)
					} else if !upToDate {
						for _, tag := range image.RepoTags {
							deleteImage(tag)
						}
					}
				}
			}
		}
	}

	danglingFilters := filters.NewArgs()
	danglingFilters.Add("dangling", "true")
	must(cli.ImagesPrune(ctx, danglingFilters))
	must(cli.ContainersPrune(ctx, filters.Args{}))
}

func deleteImage(id string) {
	cli, ctx := getDockerClient()
	items, err := cli.ImageRemove(ctx, id, types.ImageRemoveOptions{})
	if err != nil {
		printError((err.Error()))
	}
	for _, item := range items {
		if item.Untagged != "" {
			ErrPrintf("Untagged %s\n", item.Untagged)
		}
		if item.Deleted != "" {
			ErrPrintf("Deleted %s\n", item.Deleted)
		}
	}
}

// GetActualImageVersion returns the real image version stored in the environment variable TGF_IMAGE_VERSION
func GetActualImageVersion() string {
	return getActualImageVersionInternal(getImage())
}

func getDockerClient() (*client.Client, context.Context) {
	if dockerClient == nil {
		dockerClient = must(client.NewClientWithOpts(client.WithVersion(minimumDockerVersion))).(*client.Client)
		dockerContext = context.Background()
	}
	return dockerClient, dockerContext
}

var dockerClient *client.Client
var dockerContext context.Context

func getActualImageVersionInternal(imageName string) string {
	cli, ctx := getDockerClient()
	// Find image
	filters := filters.NewArgs()
	filters.Add("reference", imageName)
	images, err := cli.ImageList(ctx, types.ImageListOptions{Filters: filters})
	if err != nil || len(images) != 1 {
		return ""
	}

	return getActualImageVersionFromImageID(images[0].ID)
}

func getActualImageVersionFromImageID(imageID string) string {
	cli, ctx := getDockerClient()
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		panic(err)
	}
	for _, v := range inspect.ContainerConfig.Env {
		values := strings.SplitN(v, "=", 2)
		if values[0] == tgfImageVersion {
			return values[1]
		}
	}
	// We do not found an environment variable with the version in the images
	return ""
}

func checkImage(image string) bool {
	var out bytes.Buffer
	dockerCmd := exec.Command("docker", []string{"images", "-q", image}...)
	dockerCmd.Stdout = &out
	dockerCmd.Run()
	return out.String() != ""
}

func refreshImage(image string) {
	ErrPrintf("Checking if there is a newer version of docker image %v\n", image)
	dockerUpdateCmd := exec.Command("docker", "pull", image)
	dockerUpdateCmd.Stdout, dockerUpdateCmd.Stderr = os.Stderr, os.Stderr
	must(dockerUpdateCmd.Run())
	touchImageRefresh(image)
	ErrPrintln()
}

func getEnviron(noHome bool) (result []string) {
	for _, env := range os.Environ() {
		split := strings.Split(env, "=")
		varName := strings.TrimSpace(split[0])
		varUpper := strings.ToUpper(varName)
		if varName == "" || strings.Contains(varUpper, "PATH") {
			continue
		}

		if runtime.GOOS == "windows" {
			if strings.Contains(strings.ToUpper(split[1]), `C:\`) || strings.Contains(varUpper, "WIN") {
				continue
			}
		}

		switch varName {
		case
			"_", "PWD", "OLDPWD", "TMPDIR",
			"PROMPT", "SHELL", "SH", "ZSH", "HOME",
			"LANG", "LC_CTYPE", "DISPLAY", "TERM":
		default:
			result = append(result, "-e")
			result = append(result, split[0])
		}
	}
	return
}

// This function set the path converter function
// For old Windows version still using docker-machine and VirtualBox,
// it transforms the C:\ to /C/.
func getPathConversionFunction() func(string) string {
	if runtime.GOOS != "windows" || os.Getenv("DOCKER_MACHINE_NAME") == "" {
		return func(path string) string { return path }
	}

	return func(path string) string {
		return fmt.Sprintf("/%s%s", strings.ToUpper(path[:1]), path[2:])
	}
}

var convertDrive = getPathConversionFunction()

var windowsMessage = `
You may have to share your drives with your Docker virtual machine to make them accessible.

On Windows 10+ using Hyper-V to run Docker, simply right click on Docker icon in your tray and
choose "Settings", then go to "Shared Drives" and enable the share for the drives you want to 
be accessible to your dockers.

On previous version using VirtualBox, start the VirtualBox application and add shared drives
for all drives you want to make shareable with your dockers.

IMPORTANT, to make your drives accessible to tgf, you have to give them uppercase name corresponding
to the drive letter:
	C:\ ==> /C
	D:\ ==> /D
	...
	Z:\ ==> /Z
`
