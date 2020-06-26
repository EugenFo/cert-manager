// +skip_license_check

/*
This file contains portions of code directly taken from the 'xenolf/lego' project.
A copy of the license for this code can be found in the file named LICENSE in
this directory.
*/

// Package route53 implements a DNS provider for solving the DNS-01 challenge
// using AWS Route 53 DNS.
package route53

import (
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
	"github.com/jetstack/cert-manager/pkg/issuer/acme/dns/util"
	pkgutil "github.com/jetstack/cert-manager/pkg/util"
	"k8s.io/klog"
)

const (
	maxRetries = 5
	route53TTL = 10
)

// DNSProvider implements the util.ChallengeProvider interface
type DNSProvider struct {
	dns01Nameservers []string
	client           *route53.Route53
	hostedZoneID     string
}

type sessionProvider struct {
	AccessKeyID     string
	SecretAccessKey string
	Ambient         bool
	Region          string
	Role            string
	StsProvider     func(*session.Session) stsiface.STSAPI
}

func (d *sessionProvider) GetSession() (*session.Session, error) {
	if d.AccessKeyID == "" && d.SecretAccessKey == "" {
		if !d.Ambient {
			return nil, fmt.Errorf("unable to construct route53 provider: empty credentials; perhaps you meant to enable ambient credentials?")
		}
	} else if d.AccessKeyID == "" || d.SecretAccessKey == "" {
		// It's always an error to set one of those but not the other
		return nil, fmt.Errorf("unable to construct route53 provider: only one of access and secret key was provided")
	}

	useAmbientCredentials := d.Ambient && (d.AccessKeyID == "" && d.SecretAccessKey == "")

	config := aws.NewConfig()
	sessionOpts := session.Options{
		Config: *config,
	}

	if useAmbientCredentials {
		klog.V(5).Infof("using ambient credentials")
		// Leaving credentials unset results in a default credential chain being
		// used; this chain is a reasonable default for getting ambient creds.
		// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/configuring-sdk.html#specifying-credentials
	} else {
		klog.V(5).Infof("not using ambient credentials")
		sessionOpts.Config.Credentials = credentials.NewStaticCredentials(d.AccessKeyID, d.SecretAccessKey, "")
		// also disable 'ambient' region sources
		sessionOpts.SharedConfigState = session.SharedConfigDisable
	}

	sess, err := session.NewSessionWithOptions(sessionOpts)
	if err != nil {
		return nil, fmt.Errorf("unable to create aws session: %s", err)
	}

	if d.Role != "" {
		klog.V(5).Infof("assuming role: %s", d.Role)
		stsSvc := d.StsProvider(sess)
		result, err := stsSvc.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(d.Role),
			RoleSessionName: aws.String("cert-manager"),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to assume role: %s", err)
		}

		creds := credentials.Value{
			AccessKeyID:     *result.Credentials.AccessKeyId,
			SecretAccessKey: *result.Credentials.SecretAccessKey,
			SessionToken:    *result.Credentials.SessionToken,
		}
		sessionOpts.Config.Credentials = credentials.NewStaticCredentialsFromCreds(creds)

		sess, err = session.NewSessionWithOptions(sessionOpts)
		if err != nil {
			return nil, fmt.Errorf("unable to create aws session: %s", err)
		}
	}

	// If ambient credentials aren't permitted, always set the region, even if to
	// empty string, to avoid it falling back on the environment.
	// this has to be set after session is constructed
	if d.Region != "" || !useAmbientCredentials {
		sess.Config.WithRegion(d.Region)
	}

	sess.Handlers.Build.PushBack(request.WithAppendUserAgent(pkgutil.CertManagerUserAgent))
	return sess, nil
}

func newSessionProvider(accessKeyID, secretAccessKey, region, role string, ambient bool) (*sessionProvider, error) {
	return &sessionProvider{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		Ambient:         ambient,
		Region:          region,
		Role:            role,
		StsProvider:     defaultSTSProvider,
	}, nil
}

func defaultSTSProvider(sess *session.Session) stsiface.STSAPI {
	return sts.New(sess)
}

// NewDNSProvider returns a DNSProvider instance configured for the AWS
// Route 53 service using static credentials from its parameters or, if they're
// unset and the 'ambient' option is set, credentials from the environment.
func NewDNSProvider(accessKeyID, secretAccessKey, hostedZoneID, region, role string, ambient bool, dns01Nameservers []string) (*DNSProvider, error) {
	provider, err := newSessionProvider(accessKeyID, secretAccessKey, region, role, ambient)
	if err != nil {
		return nil, err
	}

	sess, err := provider.GetSession()
	if err != nil {
		return nil, err
	}

	client := route53.New(sess)

	return &DNSProvider{
		client:           client,
		hostedZoneID:     hostedZoneID,
		dns01Nameservers: dns01Nameservers,
	}, nil
}

// Present creates a TXT record using the specified parameters
func (r *DNSProvider) Present(domain, fqdn, value string) error {
	value = `"` + value + `"`
	return r.changeRecord(route53.ChangeActionUpsert, fqdn, value, route53TTL)
}

// CleanUp removes the TXT record matching the specified parameters
func (r *DNSProvider) CleanUp(domain, fqdn, value string) error {
	value = `"` + value + `"`
	return r.changeRecord(route53.ChangeActionDelete, fqdn, value, route53TTL)
}

func (r *DNSProvider) changeRecord(action, fqdn, value string, ttl int) error {
	hostedZoneID, err := r.getHostedZoneID(fqdn)
	if err != nil {
		return fmt.Errorf("Failed to determine Route 53 hosted zone ID: %v", err)
	}

	recordSet := newTXTRecordSet(fqdn, value, ttl)
	reqParams := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(hostedZoneID),
		ChangeBatch: &route53.ChangeBatch{
			Comment: aws.String("Managed by cert-manager"),
			Changes: []*route53.Change{
				{
					Action:            &action,
					ResourceRecordSet: recordSet,
				},
			},
		},
	}

	resp, err := r.client.ChangeResourceRecordSets(reqParams)
	if err != nil {
		if awserr, ok := err.(awserr.Error); ok {
			if action == route53.ChangeActionDelete && awserr.Code() == route53.ErrCodeInvalidChangeBatch {
				klog.V(5).Infof("ignoring InvalidChangeBatch error: %v", err)
				// If we try to delete something and get a 'InvalidChangeBatch' that
				// means it's already deleted, no need to consider it an error.
				return nil
			}
		}
		return fmt.Errorf("Failed to change Route 53 record set: %v", err)

	}

	statusID := resp.ChangeInfo.Id

	return util.WaitFor(120*time.Second, 4*time.Second, func() (bool, error) {
		reqParams := &route53.GetChangeInput{
			Id: statusID,
		}
		resp, err := r.client.GetChange(reqParams)
		if err != nil {
			return false, fmt.Errorf("Failed to query Route 53 change status: %v", err)
		}
		if *resp.ChangeInfo.Status == route53.ChangeStatusInsync {
			return true, nil
		}
		return false, nil
	})
}

func (r *DNSProvider) getHostedZoneID(fqdn string) (string, error) {
	if r.hostedZoneID != "" {
		return r.hostedZoneID, nil
	}

	authZone, err := util.FindZoneByFqdn(fqdn, r.dns01Nameservers)
	if err != nil {
		return "", fmt.Errorf("error finding zone from fqdn: %v", err)
	}

	// .DNSName should not have a trailing dot
	reqParams := &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(util.UnFqdn(authZone)),
	}
	resp, err := r.client.ListHostedZonesByName(reqParams)
	if err != nil {
		return "", err
	}

	var hostedZoneID string
	for _, hostedZone := range resp.HostedZones {
		// .Name has a trailing dot
		if !*hostedZone.Config.PrivateZone && *hostedZone.Name == authZone {
			hostedZoneID = *hostedZone.Id
			break
		}
	}

	if len(hostedZoneID) == 0 {
		return "", fmt.Errorf("Zone %s not found in Route 53 for domain %s", authZone, fqdn)
	}

	if strings.HasPrefix(hostedZoneID, "/hostedzone/") {
		hostedZoneID = strings.TrimPrefix(hostedZoneID, "/hostedzone/")
	}

	return hostedZoneID, nil
}

func newTXTRecordSet(fqdn, value string, ttl int) *route53.ResourceRecordSet {
	return &route53.ResourceRecordSet{
		Name: aws.String(fqdn),
		Type: aws.String(route53.RRTypeTxt),
		TTL:  aws.Int64(int64(ttl)),
		ResourceRecords: []*route53.ResourceRecord{
			{Value: aws.String(value)},
		},
	}
}
