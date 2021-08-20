package cmd

import (
	"fmt"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/cmd/presenters"
	"github.com/superfly/flyctl/cmdctx"
	"github.com/superfly/flyctl/docstrings"
	"github.com/superfly/flyctl/internal/client"
)

func newDomainsCommand(client *client.Client) *Command {
	domainsStrings := docstrings.Get("domains")
	cmd := BuildCommandKS(nil, nil, domainsStrings, client, nil, requireSession)

	listStrings := docstrings.Get("domains.list")
	listCmd := BuildCommandKS(cmd, runDomainsList, listStrings, client, nil, requireSession)
	listCmd.Args = cobra.MaximumNArgs(1)

	showCmd := BuildCommandKS(cmd, runDomainsShow, docstrings.Get("domains.show"), client, nil, requireSession)
	showCmd.Args = cobra.ExactArgs(1)

	addCmd := BuildCommandKS(cmd, runDomainsCreate, docstrings.Get("domains.add"), client, nil, requireSession)
	addCmd.Args = cobra.MaximumNArgs(2)

	registerCmd := BuildCommandKS(cmd, runDomainsRegister, docstrings.Get("domains.register"), client, nil, requireSession)
	registerCmd.Args = cobra.MaximumNArgs(2)

	return cmd
}

func runDomainsList(ctx *cmdctx.CmdContext) error {
	var orgSlug string
	if len(ctx.Args) == 0 {
		org, err := selectOrganization(ctx.Client.API(), "", nil)
		if err != nil {
			return err
		}
		orgSlug = org.Slug
	} else {
		// TODO: Validity check on org
		orgSlug = ctx.Args[0]
	}

	domains, err := ctx.Client.API().GetDomains(orgSlug)
	if err != nil {
		return err
	}

	if ctx.OutputJSON() {
		ctx.WriteJSON(domains)
		return nil
	}

	table := tablewriter.NewWriter(ctx.Out)

	table.SetHeader([]string{"Domain", "Registration Status", "DNS Status", "Created"})

	for _, domain := range domains {
		table.Append([]string{domain.Name, *domain.RegistrationStatus, *domain.DnsStatus, presenters.FormatRelativeTime(domain.CreatedAt)})
	}

	table.Render()

	return nil
}

func runDomainsShow(ctx *cmdctx.CmdContext) error {
	name := ctx.Args[0]

	domain, err := ctx.Client.API().GetDomain(name)
	if err != nil {
		return err
	}

	if ctx.OutputJSON() {
		ctx.WriteJSON(domain)
		return nil
	}

	ctx.Statusf("domains", cmdctx.STITLE, "Domain\n")
	fmtstring := "%-20s: %-20s\n"
	ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Name", domain.Name)
	ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Organization", domain.Organization.Slug)
	ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Registration Status", *domain.RegistrationStatus)
	if *domain.RegistrationStatus == "REGISTERED" {
		ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Expires At", presenters.FormatTime(domain.ExpiresAt))

		autorenew := ""
		if *domain.AutoRenew {
			autorenew = "Enabled"
		} else {
			autorenew = "Disabled"
		}

		ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Auto Renew", autorenew)
	}

	ctx.StatusLn()
	ctx.Statusf("domains", cmdctx.STITLE, "DNS\n")
	ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Status", *domain.DnsStatus)
	if *domain.RegistrationStatus == "UNMANAGED" {
		ctx.Statusf("domains", cmdctx.SINFO, fmtstring, "Nameservers", strings.Join(*domain.ZoneNameservers, " "))
	}

	return nil
}

func runDomainsCreate(ctx *cmdctx.CmdContext) error {
	var org *api.Organization
	var name string
	var err error

	if len(ctx.Args) == 0 {
		org, err = selectOrganization(ctx.Client.API(), "", nil)
		if err != nil {
			return err
		}

		prompt := &survey.Input{Message: "Domain name to add"}
		err := survey.AskOne(prompt, &name)
		checkErr(err)

		// TODO: Add some domain validation here
	} else if len(ctx.Args) == 2 {
		org, err = ctx.Client.API().FindOrganizationBySlug(ctx.Args[0])
		if err != nil {
			return err
		}
		name = ctx.Args[1]
	} else {
		return errors.New("specify all arguments (or no arguments to be prompted)")
	}

	fmt.Printf("Creating domain %s in organization %s\n", name, org.Slug)

	domain, err := ctx.Client.API().CreateDomain(org.ID, name)
	if err != nil {
		return err
	}

	fmt.Println("Created domain", domain.Name)

	return nil
}

func runDomainsRegister(ctx *cmdctx.CmdContext) error {
	var org *api.Organization
	var name string
	var err error

	if len(ctx.Args) == 0 {
		org, err = selectOrganization(ctx.Client.API(), "", nil)
		if err != nil {
			return err
		}

		prompt := &survey.Input{Message: "Domain name to add"}
		err := survey.AskOne(prompt, &name)
		checkErr(err)
		// TODO: Add some domain validation here
	} else if len(ctx.Args) == 2 {
		org, err = ctx.Client.API().FindOrganizationBySlug(ctx.Args[0])
		if err != nil {
			return err
		}
		name = ctx.Args[1]
	} else {
		return errors.New("specify all arguments (or no arguments to be prompted)")
	}

	checkResult, err := ctx.Client.API().CheckDomain(name)
	if err != nil {
		return err
	}

	if !checkResult.RegistrationSupported {
		return fmt.Errorf("The domain %s is not supported at this time", checkResult.DomainName)
	}

	if !checkResult.RegistrationAvailable {
		if checkResult.TransferAvailable {
			return fmt.Errorf("The domain %s is not available but can be transferred", checkResult.DomainName)
		}
		return fmt.Errorf("The domain %s is not available", checkResult.DomainName)
	}

	formattedCost := humanize.FormatFloat("", float64(checkResult.RegistrationPrice)/100)

	fmt.Printf("%s is available!\n", checkResult.DomainName)

	fmt.Printf("Registration costs $%s per year and will renew automatically after the first year.\n", formattedCost)
	fmt.Println("Your account will be charged once the domain is registered. This transaction is non-refundable.")

	if !confirm(fmt.Sprintf("Register %s for $%s?", name, formattedCost)) {
		return nil
	}

	fmt.Printf("Registering domain %s in organization %s\n", name, org.Slug)

	_, err = ctx.Client.API().CreateAndRegisterDomain(org.ID, name)
	if err != nil {
		return err
	}

	fmt.Println("Registration started")

	return nil
}
