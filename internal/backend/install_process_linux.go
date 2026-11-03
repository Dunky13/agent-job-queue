package backend

import (
	"errors"
	"os"
	"strconv"
)

func installProcessExecutable(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}

func installProcessParent(int) (int, error) {
	return 0, errors.New("process ownership checks require macOS")
}
