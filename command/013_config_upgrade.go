package command

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/internal/getproviders"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// ZeroThirteenUpgradeCommand upgrades configuration files for a module
// to include explicit provider source settings
type ZeroThirteenUpgradeCommand struct {
	Meta
}

func (c *ZeroThirteenUpgradeCommand) Run(args []string) int {
	args = c.Meta.process(args)
	flags := c.Meta.defaultFlagSet("0.13upgrade")
	flags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}

	var diags tfdiags.Diagnostics

	var dir string
	args = flags.Args()
	switch len(args) {
	case 0:
		dir = "."
	case 1:
		dir = args[0]
	default:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Too many arguments",
			"The command 0.13upgrade expects only a single argument, giving the directory containing the module to upgrade.",
		))
		c.showDiagnostics(diags)
		return 1
	}

	// Check for user-supplied plugin path
	var err error
	if c.pluginPath, err = c.loadPluginPath(); err != nil {
		c.Ui.Error(fmt.Sprintf("Error loading plugin path: %s", err))
		return 1
	}

	dir = c.normalizePath(dir)

	// Upgrade only if some configuration is present
	empty, err := configs.IsEmptyDir(dir)
	if err != nil {
		diags = diags.Append(fmt.Errorf("Error checking configuration: %s", err))
		return 1
	}
	if empty {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Not a module directory",
			fmt.Sprintf("The given directory %s does not contain any Terraform configuration files.", dir),
		))
		c.showDiagnostics(diags)
		return 1
	}

	// Set up the config loader and find all the config files
	loader, err := c.initConfigLoader()
	if err != nil {
		diags = diags.Append(err)
		c.showDiagnostics(diags)
		return 1
	}
	parser := loader.Parser()
	primary, overrides, hclDiags := parser.ConfigDirFiles(dir)
	diags = diags.Append(hclDiags)
	if diags.HasErrors() {
		c.Ui.Error(strings.TrimSpace("Failed to load configuration"))
		c.showDiagnostics(diags)
		return 1
	}

	// Load and parse all primary files
	files := make(map[string]*configs.File)
	for _, path := range primary {
		file, fileDiags := parser.LoadConfigFile(path)
		diags = diags.Append(fileDiags)
		if file != nil {
			files[path] = file
		}
	}
	if diags.HasErrors() {
		c.Ui.Error(strings.TrimSpace("Failed to load configuration"))
		c.showDiagnostics(diags)
		return 1
	}

	// FIXME: It's not clear what the correct behaviour is for upgrading
	// override files. For now, just log that we're ignoring the file.
	for _, path := range overrides {
		c.Ui.Warn(fmt.Sprintf("Ignoring override file %q: not implemented", path))
	}

	// Build up a list of required providers, uniquely by local name
	requiredProviders := make(map[string]*configs.RequiredProvider)
	var rewritePaths []string

	// Step 1: copy all explicit provider requirements across
	for path, file := range files {
		log.Printf("[DEBUG] processing required_providers from %s", path)

		for _, rps := range file.RequiredProviders {
			log.Printf("[DEBUG] found required_providers block")
			rewritePaths = append(rewritePaths, path)
			for _, rp := range rps.RequiredProviders {
				log.Printf("[DEBUG] required_provider %q", rp.Name)
				if previous, exist := requiredProviders[rp.Name]; exist {
					log.Printf("[WARN] duplicate required_provider entry found")
					diags = diags.Append(&hcl.Diagnostic{
						Summary:  "Duplicate required provider configuration",
						Detail:   fmt.Sprintf("Found duplicate required provider configuration for %q.Previously configured at %s", rp.Name, previous.DeclRange),
						Severity: hcl.DiagWarning,
						Context:  rps.DeclRange.Ptr(),
						Subject:  rp.DeclRange.Ptr(),
					})
				} else {
					// We're copying the struct here to ensure that any
					// mutation does not affect the original, if we rewrite
					// this file
					requiredProviders[rp.Name] = &configs.RequiredProvider{
						Name:        rp.Name,
						Source:      rp.Source,
						Type:        rp.Type,
						Requirement: rp.Requirement,
						DeclRange:   rp.DeclRange,
					}
					log.Printf("[DEBUG] configuration %#v", rp)
				}
			}
		}
	}

	for path, file := range files {
		log.Printf("[DEBUG] processing %s", path)
		// Step 2: add missing provider requirements from provider blocks
		for _, p := range file.ProviderConfigs {
			log.Printf("[DEBUG] provider configuration %#v", p)
			// If no explicit provider configuration exists for the
			// provider configuration's local name, add one with a legacy
			// provider address.
			if _, exist := requiredProviders[p.Name]; !exist {
				log.Printf("[DEBUG] no required providers entry found for %q, adding one", p.Name)
				requiredProviders[p.Name] = &configs.RequiredProvider{
					Name:        p.Name,
					Type:        addrs.NewLegacyProvider(p.Name),
					Requirement: p.Version,
				}
			}
		}

		// Step 3: add missing provider requirements from resources
		resources := [][]*configs.Resource{file.ManagedResources, file.DataResources}
		for _, rs := range resources {
			for _, r := range rs {
				log.Printf("[DEBUG] resource %s", r.Addr())

				// Find the appropriate provider local name for this resource
				var localName string

				// If there's a provider config, use that to determine the
				// local name. Otherwise use the implied provider local name
				// based on the resource's address.
				if r.ProviderConfigRef != nil {
					localName = r.ProviderConfigRef.Name
				} else {
					localName = r.Addr().ImpliedProvider()
				}
				log.Printf("[DEBUG] resource provider local name is %q", localName)

				// If no explicit provider configuration exists for this local
				// name, add one with a legacy provider address.
				if _, exist := requiredProviders[localName]; !exist {
					log.Printf("[DEBUG] no required providers entry found for %q, adding one", localName)
					requiredProviders[localName] = &configs.RequiredProvider{
						Name: localName,
						Type: addrs.NewLegacyProvider(localName),
					}
				}
			}
		}
	}

	// We should now have a complete understanding of the provider requirements
	// stated in the config.  If there are any providers, attempt to detect
	// their sources, and rewrite the config.
	if len(requiredProviders) > 0 {
		detectDiags := c.detectProviderSources(requiredProviders)
		diags = diags.Append(detectDiags)
		if diags.HasErrors() {
			c.Ui.Error("Unable to detect sources for providers")
			c.showDiagnostics(diags)
			return 1
		}

		// FIXME
		if len(rewritePaths) != 1 {
			c.Ui.Error("Not implemented")
			c.showDiagnostics(diags)
			return 1
		}

		// Load and parse the output configuration file
		filename := rewritePaths[0]
		config, err := ioutil.ReadFile(filename)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to read configuration file",
				fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		}
		out, parseDiags := hclwrite.ParseConfig(config, filename, hcl.InitialPos)
		diags = diags.Append(parseDiags)
		if diags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}

		// Find all required_providers blocks, and store them alongside a map
		// back to the parent terraform block.
		var requiredProviderBlocks []*hclwrite.Block
		parentBlocks := make(map[*hclwrite.Block]*hclwrite.Block)
		root := out.Body()
		for _, rootBlock := range root.Blocks() {
			if rootBlock.Type() != "terraform" {
				continue
			}
			for _, childBlock := range rootBlock.Body().Blocks() {
				if childBlock.Type() == "required_providers" {
					requiredProviderBlocks = append(requiredProviderBlocks, childBlock)
					parentBlocks[childBlock] = rootBlock
				}
			}
		}

		first, rest := requiredProviderBlocks[0], requiredProviderBlocks[1:]

		// Find the body of the first block to prepare for rewriting it
		body := first.Body()

		// Build a sorted list of provider local names
		var localNames []string
		for localName := range requiredProviders {
			localNames = append(localNames, localName)
		}
		sort.Strings(localNames)

		// Populate the required providers block
		for _, localName := range localNames {
			requiredProvider := requiredProviders[localName]
			var attributes = make(map[string]cty.Value)

			if !requiredProvider.Type.IsZero() {
				attributes["source"] = cty.StringVal(requiredProvider.Type.String())
			}

			if version := requiredProvider.Requirement.Required.String(); version != "" {
				attributes["version"] = cty.StringVal(version)
			}

			body.SetAttributeValue(localName, cty.MapVal(attributes))

			// FIXME: how do we add the comment if there's no source?
		}

		// Remove the rest of the blocks (and the parent block, if it's empty)
		for _, rpBlock := range rest {
			tfBlock := parentBlocks[rpBlock]
			tfBody := tfBlock.Body()
			tfBody.RemoveBlock(rpBlock)

			// If the terraform block has no blocks and no attributes, it's
			// basically empty (aside from comments and whitespace), so it's
			// more useful to remove it than leave it in.
			if len(tfBody.Blocks()) == 0 && len(tfBody.Attributes()) == 0 {
				root.RemoveBlock(tfBlock)
			}
		}

		// Write the config back to the file
		f, err := os.OpenFile(filename, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, os.ModePerm)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to open configuration file for writing",
				fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		}
		_, err = out.WriteTo(f)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to rewrite configuration file",
				fmt.Sprintf("Error when rewriting configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		}
	}

	c.showDiagnostics(diags)
	if diags.HasErrors() {
		return 1
	}

	if len(diags) != 0 {
		c.Ui.Output(`-----------------------------------------------------------------------------`)
	}
	c.Ui.Output(c.Colorize().Color(`
[bold][green]Upgrade complete![reset]

Use your version control system to review the proposed changes, make any
necessary adjustments, and then commit.
`))

	return 0
}

// For providers which need a source attribute, detect the source
func (c *ZeroThirteenUpgradeCommand) detectProviderSources(requiredProviders map[string]*configs.RequiredProvider) tfdiags.Diagnostics {
	source := c.providerInstallSource()
	var diags tfdiags.Diagnostics

	for name, rp := range requiredProviders {
		log.Printf("[DEBUG] detecting source for %#v", rp)

		// If there's already an explicit source, skip it
		if rp.Source != "" {
			log.Printf("[DEBUG] source present, skipping")
			continue
		}

		// Construct a legacy provider FQN using the existing addr's type. This
		// is necessary because the config parser for required providers
		// constructs a default provider FQN for configurations with no source.
		// For this tool specifically we want to treat those as legacy
		// providers, so that we can look up the namespace on the registry.
		addr := addrs.NewLegacyProvider(rp.Type.Type)
		p, err := getproviders.LookupLegacyProvider(addr, source)
		if err == nil {
			log.Printf("[DEBUG] detected provider source for %q: %q", rp.Name, p)
			rp.Type = p
		} else {
			if _, ok := err.(getproviders.ErrProviderNotKnown); ok {
				log.Printf("[DEBUG] provider not found for %s, marking as missing", addr)

				// Setting the provider address to a zero value struct
				// indicates that there is no known FQN for this provider,
				// which will cause us to write an explanatory comment in the
				// HCL output advising the user what to do about this.
				rp.Type = addrs.Provider{}
			} else {
				log.Printf("[DEBUG] unknown error looking up %q: %s", addr, err)
			}
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Could not detect provider source",
				fmt.Sprintf("Error looking up provider source for %q: %s", name, err),
			))
		}
	}

	return diags
}

func noSourceDetectedComment(name string) string {
	return fmt.Sprintf(`# TF-UPGRADE-TODO
#
# No source detected for this provider. You must add a source address
# in the following format:
#
# source = "your.domain.com/organization/%s"
#
# For more information, see the provider source documentation:
#
# https://www.terraform.io/docs/configuration/providers.html#provider-source
`, name)
}

func (c *ZeroThirteenUpgradeCommand) Help() string {
	helpText := `
Usage: terraform 0.13upgrade [module-dir]

  Generates a "providers.tf" configuration file which includes source
  configuration for every non-default provider.
`
	return strings.TrimSpace(helpText)
}

func (c *ZeroThirteenUpgradeCommand) Synopsis() string {
	return "Rewrites pre-0.13 module source code for v0.13"
}
