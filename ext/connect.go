package ext

import (
	"cmp"
	"context"
	"flag"
	"slices"
)

const DefaultBastionImage = "projects/debian-cloud/global/images/family/debian-13"

type Bastion struct {
	Spot        bool
	Subnet      string
	MachineType string
	Image       string
}

type Config struct {
	Project  string
	Service  string
	Instance string
	Endpoint string
	Network  string
	Region   string
	Zone     string
	Duration string
	Output   string
	Port     int
	NoInput  bool
	Cmd      []string
	Bastion  Bastion
	Options  map[string]string

	EndOnStdinClose bool
}

func (c Config) Option(key string) string {
	return c.Options[key]
}

func (c *Config) SetOption(key, value string) {
	if c.Options == nil {
		c.Options = map[string]string{}
	}
	if value == "" {
		delete(c.Options, key)
		return
	}
	c.Options[key] = value
}

type Question struct {
	Title  string
	Bool   *bool
	Text   *string
	Commit func()
}

type ConnectHooks struct {
	Order         int
	Flags         func(fs *flag.FlagSet, c *Config)
	TargetFlags   func(fs *flag.FlagSet, c *Config)
	Validate      func(c Config, explicit map[string]bool) error
	Expand        func(positional string, c Config, explicit map[string]bool) ([]Config, bool, error)
	Start         func(ctx context.Context) (Config, bool, error)
	Wizard        func(c *Config) []Question
	BeforeAuth    func(set, cfgs []Config) []string
	BeforeBastion func(cfgs []Config, explicit map[string]bool)
	Resolve       func(ctx context.Context, h Host, c Config) (t Target, handled bool, err error)
}

type Command struct {
	Name     string
	Usage    []string
	Run      func(args []string) error
	Complete func(args []string) []string
}

type Core interface {
	ParseFlags(fs *flag.FlagSet, command, usageLine string, args []string) (help bool, err error)
	TargetFlags(fs *flag.FlagSet, c *Config)
	Validate(c Config, explicit map[string]bool) error
	Connect(cfgs []Config, explicit map[string]bool) error
	Wizard(ctx context.Context, prefill Config) (Config, error)
	Pick(ctx context.Context, title string, options []Choice) (string, error)
}

var (
	connectHooks []ConnectHooks
	commands     = map[string]Command{}
	core         Core
)

func RegisterConnect(h ConnectHooks) {
	mu.Lock()
	defer mu.Unlock()
	connectHooks = append(connectHooks, h)
}

func Connect() []ConnectHooks {
	mu.RLock()
	defer mu.RUnlock()
	out := append([]ConnectHooks(nil), connectHooks...)
	slices.SortStableFunc(out, func(a, b ConnectHooks) int { return cmp.Compare(a.Order, b.Order) })
	return out
}

func FlagRegistered(name string) bool {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	var c Config
	for _, h := range Connect() {
		if h.Flags != nil {
			h.Flags(fs, &c)
		}
		if h.TargetFlags != nil {
			h.TargetFlags(fs, &c)
		}
	}
	return fs.Lookup(name) != nil
}

func RegisterCommand(c Command) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := commands[c.Name]; dup {
		panic("ext: command " + c.Name + " registered twice")
	}
	commands[c.Name] = c
}

func LookupCommand(name string) (Command, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := commands[name]
	return c, ok
}

func Commands() []Command {
	mu.RLock()
	defer mu.RUnlock()
	var out []Command
	for _, c := range commands {
		out = append(out, c)
	}
	return out
}

func SetCore(c Core) {
	mu.Lock()
	defer mu.Unlock()
	core = c
}

func CoreAPI() Core {
	mu.RLock()
	defer mu.RUnlock()
	return core
}
