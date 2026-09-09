package main

func homogeneousDarwinCPU() error {
	_, _, err := x86CPUKind()
	if err != nil {
		return err
	}
	_, _, _, features := cpuid(7, 0)
	if features&(1<<15) != 0 {
		return errHybridDarwin
	}
	return nil
}
