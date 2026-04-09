package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var regions = []string{
	"us-east-1",
	"us-east-2",
	"us-west-2",
	"ca-west-1",
}

type clusterInfo struct {
	Profile  string
	Region   string
	Name     string
	Endpoint string
	CAData   []byte
}

func main() {
	dryRun := flag.Bool("dry-run", false, "write kubeconfig to a temp file instead of ~/.kube/config")
	accountPrefix := flag.String("account-prefix", "", "only include profiles starting with this prefix")
	role := flag.String("role", "", "only include profiles with this sso_role_name (exact match)")
	flag.Parse()

	profiles, err := parseAWSProfiles(*accountPrefix, *role)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading AWS profiles: %v\n", err)
		os.Exit(1)
	}

	if len(profiles) == 0 {
		fmt.Fprintln(os.Stderr, "no profiles found in ~/.aws/config")
		os.Exit(1)
	}

	fmt.Printf("Found %d profiles: %s\n", len(profiles), strings.Join(profiles, ", "))

	type regionResult struct {
		Region   string
		Clusters []clusterInfo
		Err      error
	}

	type profileResult struct {
		Profile string
		Results []regionResult
	}

	// Semaphore to cap concurrent AWS API calls and avoid SSO cache contention.
	const maxConcurrency = 20
	sem := make(chan struct{}, maxConcurrency)
	fmt.Printf("\nScanning with concurrency limit of %d workers...\n", maxConcurrency)

	var (
		mu             sync.Mutex
		profileResults []profileResult
		wg             sync.WaitGroup
	)

	for _, profile := range profiles {
		wg.Add(1)
		go func(profile string) {
			defer wg.Done()
			var results []regionResult
			var innerWg sync.WaitGroup
			var innerMu sync.Mutex

			for _, region := range regions {
				innerWg.Add(1)
				go func(region string) {
					defer innerWg.Done()
					sem <- struct{}{}
					found, err := discoverClusters(profile, region)
					<-sem
					innerMu.Lock()
					results = append(results, regionResult{Region: region, Clusters: found, Err: err})
					innerMu.Unlock()
				}(region)
			}

			innerWg.Wait()

			// Sort results by region order.
			regionOrder := make(map[string]int)
			for i, r := range regions {
				regionOrder[r] = i
			}
			sort.Slice(results, func(i, j int) bool {
				return regionOrder[results[i].Region] < regionOrder[results[j].Region]
			})

			mu.Lock()
			profileResults = append(profileResults, profileResult{Profile: profile, Results: results})
			mu.Unlock()
		}(profile)
	}

	wg.Wait()

	// Sort by original profile order.
	profileOrder := make(map[string]int)
	for i, p := range profiles {
		profileOrder[p] = i
	}
	sort.Slice(profileResults, func(i, j int) bool {
		return profileOrder[profileResults[i].Profile] < profileOrder[profileResults[j].Profile]
	})

	// Print grouped output and collect all clusters.
	var clusters []clusterInfo
	for _, pr := range profileResults {
		fmt.Printf("\n[%s]\n", pr.Profile)
		for _, rr := range pr.Results {
			if rr.Err != nil {
				fmt.Printf("    [%s] %v\n", rr.Region, rr.Err)
			}
			for _, c := range rr.Clusters {
				fmt.Printf("    [%s] %s\n", rr.Region, c.Name)
				clusters = append(clusters, c)
			}
		}
	}
	fmt.Println()

	if len(clusters) == 0 {
		fmt.Println("No EKS clusters found.")
		return
	}

	if err := updateKubeconfig(clusters, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "error updating kubeconfig: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Done.")
}

// parseAWSProfiles reads ~/.aws/config and returns all profile names,
// optionally filtered by a name prefix and/or sso_role_name.
func parseAWSProfiles(prefix, role string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	f, err := os.Open(filepath.Join(home, ".aws", "config"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type profileEntry struct {
		name    string
		ssoRole string
	}

	var entries []profileEntry
	var current *profileEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[profile ") && strings.HasSuffix(line, "]") {
			if current != nil {
				entries = append(entries, *current)
			}
			name := strings.TrimSuffix(strings.TrimPrefix(line, "[profile "), "]")
			current = &profileEntry{name: name}
		} else if current != nil && strings.HasPrefix(line, "sso_role_name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				current.ssoRole = strings.TrimSpace(parts[1])
			}
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var profiles []string
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.name, prefix) {
			continue
		}
		if role != "" && e.ssoRole != role {
			continue
		}
		profiles = append(profiles, e.name)
	}
	return profiles, nil
}

// discoverClusters lists and describes all EKS clusters for a given profile/region.
func discoverClusters(profile, region string) ([]clusterInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	eksClient := eks.NewFromConfig(cfg)

	// Retry with exponential backoff to handle transient SSO cache contention
	// when many profiles refresh tokens concurrently.
	const maxAttempts = 5
	var listOut *eks.ListClustersOutput
	for attempt := range maxAttempts {
		listOut, err = eksClient.ListClusters(ctx, &eks.ListClustersInput{})
		if err == nil {
			break
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "ForbiddenException") || strings.Contains(errMsg, "No access") {
			return nil, fmt.Errorf("no access (you may not have permissions for this account)")
		}
		if strings.Contains(errMsg, "token is expired") || strings.Contains(errMsg, "Token has expired") || strings.Contains(errMsg, "SSO session") {
			return nil, fmt.Errorf("SSO session expired (run: aws sso login --profile %s)", profile)
		}
		if strings.Contains(errMsg, "failed to refresh cached credentials") {
			if attempt < maxAttempts-1 {
				backoff := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
				fmt.Printf("    [%s/%s] credential contention, retrying in %s (attempt %d/%d)...\n",
					profile, region, backoff, attempt+1, maxAttempts)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("failed to refresh credentials after %d attempts (try: aws sso login --profile %s): %w",
				maxAttempts, profile, err)
		}
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	if len(listOut.Clusters) == 0 {
		return nil, nil
	}

	var clusters []clusterInfo
	for _, name := range listOut.Clusters {
		if strings.HasPrefix(name, "sre") {
			continue
		}
		descOut, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
			Name: aws.String(name),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s/%s] describe %s: %v\n", profile, region, name, err)
			continue
		}

		caData, err := base64.StdEncoding.DecodeString(aws.ToString(descOut.Cluster.CertificateAuthority.Data))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s/%s] decode CA for %s: %v\n", profile, region, name, err)
			continue
		}

		clusters = append(clusters, clusterInfo{
			Profile:  profile,
			Region:   region,
			Name:     name,
			Endpoint: aws.ToString(descOut.Cluster.Endpoint),
			CAData:   caData,
		})
	}

	return clusters, nil
}

// updateKubeconfig merges discovered clusters into ~/.kube/config (or a temp file in dry-run mode).
func updateKubeconfig(clusters []clusterInfo, dryRun bool) error {
	kubeConfigPath := clientcmd.RecommendedHomeFile

	if dryRun {
		f, err := os.CreateTemp("", "kubeconfig-*.yaml")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		f.Close()
		kubeConfigPath = f.Name()
		fmt.Printf("Dry run: writing to %s\n", kubeConfigPath)
	}

	// Load existing kubeconfig or start fresh.
	existingConfig, err := clientcmd.LoadFromFile(kubeConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("load kubeconfig: %w", err)
		}
		existingConfig = clientcmdapi.NewConfig()
	}

	for _, c := range clusters {
		clusterKey := c.Name
		userKey := c.Name

		existingConfig.Clusters[clusterKey] = &clientcmdapi.Cluster{
			Server:                   c.Endpoint,
			CertificateAuthorityData: c.CAData,
		}

		existingConfig.AuthInfos[userKey] = &clientcmdapi.AuthInfo{
			Exec: &clientcmdapi.ExecConfig{
				APIVersion: "client.authentication.k8s.io/v1beta1",
				Command:    "aws",
				Args: []string{
					"eks", "get-token",
					"--cluster-name", c.Name,
					"--region", c.Region,
				},
				Env: []clientcmdapi.ExecEnvVar{
					{Name: "AWS_PROFILE", Value: c.Profile},
				},
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			},
		}

		existingConfig.Contexts[c.Name] = &clientcmdapi.Context{
			Cluster:  clusterKey,
			AuthInfo: userKey,
		}

	}

	if err := clientcmd.WriteToFile(*existingConfig, kubeConfigPath); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	return nil
}
