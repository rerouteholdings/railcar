package audio

// #include <stdlib.h>
import "C"

import "unsafe"

// cfree frees memory malgo allocated in C, such as a copied device ID.
func cfree(p unsafe.Pointer) { C.free(p) }
