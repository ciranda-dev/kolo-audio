package libopus

// MaxMem is the memory limit in 64 KiB pages that the generator wrote.
const MaxMem = maxMem

// StackTop returns the initial shadow stack pointer.
func StackTop(m *Module) int32 { return m.g0 }
