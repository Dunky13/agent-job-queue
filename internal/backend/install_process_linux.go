package backend

import (
	"os"
	"strconv"
)

func installProcessExecutable(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}
