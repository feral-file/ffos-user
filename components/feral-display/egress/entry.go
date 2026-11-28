package main

import "C"
import (
	"runtime"
	"unsafe"
)

//export ff_network
func ff_network(argc C.int, argv **C.char) C.int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	args := make([]string, int(argc))
	for i, arg := range unsafe.Slice(argv, int(argc)) {
		args[i] = C.GoString(arg)
	}
	run(args)
	return 0
}

func main() {}
