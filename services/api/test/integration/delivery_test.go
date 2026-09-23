package integration_test

import "github.com/Paca-AI/api/internal/platform/delivery"

func openTaskGate() *delivery.Gate {
	g := delivery.NewGate()
	_ = g.Update("0", false)
	return g
}
