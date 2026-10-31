package cli

import (
	"context"
	"flag"

	"github.com/charmbracelet/huh"

	"github.com/pilef-co/bqto/ext"
)

func init() {
	ext.SetCore(coreAPI{})
}

type coreAPI struct{}

func (coreAPI) ParseFlags(fs *flag.FlagSet, command, usageLine string, args []string) (bool, error) {
	return parseFlags(fs, command, usageLine, args)
}

func (coreAPI) TargetFlags(fs *flag.FlagSet, c *ext.Config) {
	registerTargetFlags(fs, c)
}

func (coreAPI) Validate(c ext.Config, explicit map[string]bool) error {
	for _, f := range ext.ProFlags() {
		if explicit[f.Name] && !ext.FlagRegistered(f.Name) {
			return ext.ProError{What: "--" + f.Name, Drop: "drop --" + f.Name}
		}
	}
	if err := checkPortZero(explicit, c.Port); err != nil {
		return err
	}
	cfg := fromExt(c)
	check := cfg
	if _, err := validateConfig(&check); err != nil {
		return err
	}
	if err := validateValues(cfg); err != nil {
		return err
	}
	return validateHooks(cfg, explicit)
}

func (coreAPI) Connect(cfgs []ext.Config, explicit map[string]bool) error {
	out := make([]connectConfig, len(cfgs))
	for i, c := range cfgs {
		out[i] = fromExt(c)
	}
	return doConnect(out, explicit)
}

func (coreAPI) Wizard(ctx context.Context, prefill ext.Config) (ext.Config, error) {
	cfg, err := runWizard(ctx, fromExt(prefill))
	return toExt(cfg), err
}

func (coreAPI) Pick(ctx context.Context, title string, options []ext.Choice) (string, error) {
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o.Label, o.Value)
	}
	return pickOne(ctx, title, opts)
}
