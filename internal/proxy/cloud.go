package proxy

import (
	"log"
	"sync"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/config"
)

type cloudControl struct {
	mu            sync.Mutex
	configured    config.CloudMode
	effective     config.CloudMode
	disabledByHUP bool
	budgetLogDay  string
}

func newCloudControl(mode config.CloudMode) *cloudControl {
	return &cloudControl{
		configured: mode,
		effective:  mode,
	}
}

func (c *cloudControl) EffectiveMode() config.CloudMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effective
}

func (c *cloudControl) HandleSIGHUP() config.CloudMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.effective == config.CloudOff && c.disabledByHUP {
		c.effective = c.configured
		c.disabledByHUP = false
		log.Printf("cloud egress restored via SIGHUP")
		return c.effective
	}
	c.effective = config.CloudOff
	c.disabledByHUP = true
	log.Printf("cloud egress DISABLED via SIGHUP")
	return c.effective
}

func (c *cloudControl) LogBudgetBlockOnce(now time.Time, capMicro int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	day := utcDay(now.UTC())
	if c.budgetLogDay == day {
		return
	}
	c.budgetLogDay = day
	log.Printf("bursty cloud: daily cloud spend budget exhausted for %s (cap $%s; unpriced cloud usage counts $0 toward the cap)", day, formatUSDLog(capMicro))
}
