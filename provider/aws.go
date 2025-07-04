package provider

import (
	"context"
	"encoding/json"
	"errors"
	"k8f/core"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	log "github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
)

// to check if the profile is valid or not we need to load the []AwsProfiles and check if the profile is a Assume role or credentials, and do a simple call to aws to validate it works
func validateCredentials(creds []AwsProfiles) (string, error) {
	log.Info("Validating AWS Credentials")
	for _, profile := range creds {
		var svc *sts.Client
		var conf aws.Config
		var err error
		conf, err = config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(profile.Name))
		if err != nil {
			return profile.Name, err
		}
		if profile.IsRole {
			conf.Credentials = stsAssumeRole(profile)
			svc = sts.NewFromConfig(conf)
		} else {
			svc = sts.NewFromConfig(conf)
		}
		var input *sts.GetCallerIdentityInput = &sts.GetCallerIdentityInput{}
		_, err = svc.GetCallerIdentity(context.Background(), input)
		if err != nil {
			return profile.Name, err
		}
	}
	log.Info("AWS Credentials Validated")
	return "", nil
}

// https://docs.aws.amazon.com/sdk-for-go/api/aws/credentials/stscreds/#:~:text=or%20service%20clients.-,Assume%20Role,-To%20assume%20an
func (c CommandOptions) FullAwsList() Provider {
	var f []Account
	core.CheckEnvVarOrSitIt("AWS_REGION", c.AwsRegion)
	profiles := c.GetLocalAwsProfiles()
	if len(profiles) == 0 {
		core.FailOnError(errors.New("no profiles to run with"), "process failed with Error")
	}
	addOnVersion := profiles[0].getVersion()
	l := getLatestEKS(getEKSversionsList(addOnVersion))
	log.Trace(profiles)
	c0 := make(chan Account)
	for _, profile := range profiles {
		go func(c0 chan Account, profile AwsProfiles, l string) {
			var re []Cluster
			log.Info(string("Using AWS profile: " + profile.Name))
			regions := profile.listRegions()
			c2 := make(chan []Cluster)
			for _, reg := range regions {
				go printOutResult(reg, l, profile, addOnVersion, c2)
			}
			for i := 0; i < len(regions); i++ {
				aRegion := <-c2
				if len(aRegion) > 0 {
					re = append(re, aRegion...)
				}
			}
			c0 <- Account{profile.Name, re, len(re), ""}
		}(c0, profile, l)
	}
	for i := 0; i < len(profiles); i++ {
		res := <-c0
		if len(res.Clusters) != 0 {
			f = append(f, res)
		}
	}
	return Provider{"aws", f, countTotal(f)}
}

// get Addons Supported EKS versions
func (p AwsProfiles) getVersion() *eks.DescribeAddonVersionsOutput {
	var svc *eks.Client
	conf, err := config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(p.ConfProfile))
	core.FailOnError(err, "Failed to get Version")
	if p.IsRole {
		conf.Credentials = stsAssumeRole(p)
		svc = eks.NewFromConfig(conf)
	} else {
		svc = eks.NewFromConfig(conf)
	}
	input2 := &eks.DescribeAddonVersionsInput{}
	r, err := svc.DescribeAddonVersions(context.TODO(), input2)
	core.FailOnError(err, "Failed to get Describe Version with profile: "+p.ConfProfile)
	return r
}

// gets the latest form suppported Addons
func getLatestEKS(addons []string) string {
	return evaluateVersion(addons)
}

// create Version list
func getEKSversionsList(addons *eks.DescribeAddonVersionsOutput) []string {
	var supportList []string
	for _, a := range addons.Addons {
		for _, c := range a.AddonVersions {
			for _, v := range c.Compatibilities {
				if !core.IfXinY(*v.ClusterVersion, supportList) {
					supportList = append(supportList, *v.ClusterVersion)
				}
			}
		}
	}
	return supportList
}

// get installed Version on existing Clusters
func (p AwsProfiles) getEksCurrentVersion(cluster string, profile AwsProfiles, reg string, c3 chan []string) {
	var svc *eks.Client
	var conf aws.Config
	var err error
	conf, err = config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(profile.ConfProfile))
	core.FailOnError(err, awsErrorMessage)
	if p.IsRole {
		conf.Credentials = stsAssumeRole(p)
		svc = eks.NewFromConfig(conf, func(o *eks.Options) {
			o.Region = reg
		})
	} else {
		svc = eks.NewFromConfig(conf, func(o *eks.Options) {
			o.Region = reg
		})
	}
	input := &eks.DescribeClusterInput{
		Name: aws.String(cluster),
	}
	result, err := svc.DescribeCluster(context.TODO(), input)
	core.FailOnError(err, "Failed to Get Cluster Info")
	c3 <- []string{cluster, *result.Cluster.Version}
}

// get all Regions avilable
func (p AwsProfiles) listRegions() []string {
	core.CheckEnvVarOrSitIt("AWS_REGION", Kregion)
	var reg []string
	var svc *ec2.Client
	var conf aws.Config
	var err error
	conf, err = config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(p.Name))
	if err != nil {
		log.Warnf("Skipping profile %s: failed to load config for region listing: %v", p.Name, err)
		return reg
	}
	if p.IsRole {
		conf.Credentials = stsAssumeRole(p)
		svc = ec2.NewFromConfig(conf)
	} else {
		svc = ec2.NewFromConfig(conf)
	}
	input := &ec2.DescribeRegionsInput{}
	result, err := svc.DescribeRegions(context.TODO(), input)
	if err != nil {
		log.Warnf("Skipping profile %s: failed to get region info: %v", p.Name, err)
		return reg
	}
	log.Debugf("Using profile: %s, ARN: %s, IsRole:%t", p.Name, p.Arn, p.IsRole)
	for _, r := range result.Regions {
		if r.OptInStatus != nil && (*r.OptInStatus == "opted-in" || *r.OptInStatus == "opt-in-not-required") {
			reg = append(reg, *r.RegionName)
		}
	}

	return reg
}

func printOutResult(reg string, latest string, profile AwsProfiles, addons *eks.DescribeAddonVersionsOutput, c chan []Cluster) {
	var loc []Cluster
	var svc *eks.Client
	var conf aws.Config
	var err error
	conf, err = config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(profile.ConfProfile))
	if err != nil {
		log.Warnf("Skipping region %s for profile %s: failed to load config: %v", reg, profile.Name, err)
		c <- loc
		return
	}
	if profile.IsRole {
		conf.Credentials = stsAssumeRole(profile)
		svc = eks.NewFromConfig(conf, func(o *eks.Options) {
			o.Region = reg
		})
	} else {
		svc = eks.NewFromConfig(conf, func(o *eks.Options) {
			o.Region = reg
		})
	}
	input := &eks.ListClustersInput{}
	result, err := svc.ListClusters(context.TODO(), input)
	if err != nil {
		log.Warnf("Skipping region %s for profile %s: failed to list clusters: %v", reg, profile.Name, err)
		c <- loc
		return
	}
	log.Debug(string("We are In Region: " + reg + " Profile " + profile.Name))
	if len(result.Clusters) > 0 {
		c3 := make(chan []string)
		for _, element := range result.Clusters {
			go profile.getEksCurrentVersion(element, profile, reg, c3)
		}
		for i := 0; i < len(result.Clusters); i++ {
			res := <-c3
			loc = append(loc, Cluster{res[0], res[1], latest, reg, "", "", HowManyVersionsBack(getEKSversionsList(addons), res[1])})
		}
	}
	c <- loc
}

func (c CommandOptions) GetLocalAwsProfiles() []AwsProfiles {
	var arr []AwsProfiles
	mergeconf, err := core.MergeINIFiles([]string{config.DefaultSharedConfigFilename(), config.DefaultSharedCredentialsFilename()})
	core.FailOnError(err, "failed to merge INI")
	creds, err := ini.Load(mergeconf)
	core.FailOnError(err, "Failed to load profile from creds")
	for _, p := range creds.Sections() {
		if len(p.Keys()) != 0 {
			profileName := removeString("profile", p.Name())
			_, isInArray := XinAwsProfiles(profileName, arr)
			kbool, karn := checkIfItsAssumeRole(p.Keys())
			if kbool && !isInArray {
				arr = append(arr, AwsProfiles{Name: profileName, IsRole: true, Arn: karn, ConfProfile: p.Name()})
			} else {
				arr = append(arr, AwsProfiles{Name: profileName, IsRole: false, ConfProfile: p.Name()})
			}
		}
	}
	// add a if statement to check if to fail on validation error or not
	fultyProfile, err := validateCredentials(arr)
	arr = c.finalizeValidation(arr, fultyProfile, err)
	kJson, _ := json.Marshal(arr)
	log.Debugf("profile in use: %s", string(kJson))
	return (arr) // Create JSON string response
}

func (c CommandOptions) finalizeValidation(arr []AwsProfiles, fultyProfile string, err error) []AwsProfiles {
	if c.Validate {
		core.FailOnError(err, credentialsValidationError+fultyProfile)
	} else {
		if err != nil {
			log.Error(credentialsValidationError + fultyProfile)
			for i, v := range arr {
				if v.Name == fultyProfile {
					arr = append(arr[:i], arr[i+1:]...)
				}
			}
			log.Warn(fultyProfile + " was removed from the list of profiles")
		}
	}
	return arr
}

// Connect Logic
func (c CommandOptions) ConnectAllEks() AllConfig {
	var auth []Users
	var contexts []Contexts
	var clusters []Clusters
	var arnContext string
	var distinctArnContexts []string
	core.CheckEnvVarOrSitIt("AWS_REGION", c.AwsRegion)
	p := c.FullAwsList()
	awsProfiles := c.GetLocalAwsProfiles()
	for _, a := range p.Accounts {
		r := make(chan LocalConfig)
		for _, clus := range a.Clusters {
			go func(r chan LocalConfig, clus Cluster, a Account, commandOptions CommandOptions, awsProfiles []AwsProfiles) {
				var eksSvc *eks.Client
				var conf aws.Config
				var err error
				inProfile, _ := XinAwsProfiles(a.Name, awsProfiles)
				log.Infof("The Profile used is %s, and region is %s", awsProfiles[inProfile].Name, clus.Region)
				conf, err = config.LoadDefaultConfig(context.TODO(), config.WithSharedConfigProfile(awsProfiles[inProfile].Name))
				core.FailOnError(err, awsErrorMessage)
				if awsProfiles[inProfile].IsRole {
					conf.Credentials = stsAssumeRole(awsProfiles[inProfile])
					eksSvc = eks.NewFromConfig(conf, func(o *eks.Options) {
						o.Region = clus.Region
					})
				} else {
					eksSvc = eks.NewFromConfig(conf, func(o *eks.Options) {
						o.Region = clus.Region
					})
				}
				input := &eks.DescribeClusterInput{
					Name: aws.String(clus.Name),
				}
				result, err := eksSvc.DescribeCluster(context.TODO(), input)
				log.Debugf("the Profile that is used is: %s", awsProfiles[inProfile].Name)
				core.FailOnError(err, "Error calling DescribeCluster")
				r <- GenerateKubeConfiguration(result.Cluster, clus.Region, a, commandOptions)
			}(r, clus, a, c, awsProfiles)
		}
		for i := 0; i < len(a.Clusters); i++ {
			result := <-r
			duplicationFound := false
			// below check ensures we do not let duplicate clusters/contexts/users
			// get into the final configuration in case if there are different
			// aws profiles configured to assume the same role, because any kind of
			// a duplicate (cluster, context, user) makes kubectl crash
			for _, s := range distinctArnContexts {
				if s == result.Context.Cluster {
					duplicationFound = true
					break
				}
			}
			if duplicationFound {
				continue
			}
			arnContext = result.Context.Cluster
			distinctArnContexts = append(distinctArnContexts, result.Context.Cluster)
			auth = append(auth, Users{Name: arnContext, User: result.Authinfo})
			contexts = append(contexts, Contexts{Name: arnContext, Context: result.Context})
			clusters = append(clusters, Clusters{Name: arnContext, Cluster: result.Cluster})
		}
	}
	if c.Combined {
		log.Println("Started aws only config creation")
		c.CombineConfigs(AllConfig{auth, contexts, clusters}, arnContext)
		return AllConfig{}
	}
	log.Println("Started aws combined config creation")
	return AllConfig{auth, contexts, clusters}
}

// Create AWS Config
func GenerateKubeConfiguration(cluster *types.Cluster, r string, a Account, c CommandOptions) LocalConfig {
	clusterName := c.SetClusterName(cluster.Arn)
	clusters := CCluster{
		Server:                   *cluster.Endpoint,
		CertificateAuthorityData: *cluster.CertificateAuthority.Data,
	}
	contexts := Context{
		Cluster: clusterName,
		User:    clusterName,
	}

	authinfos := User{
		Exec: Exec{
			APIVersion: "client.authentication.k8s.io/v1beta1",
			Args:       c.AwsArgs(r, *cluster.Name, *cluster.Arn),
			Env:        c.AwsEnvs(a.Name),
			Command:    c.setCommand(),
		},
	}
	return LocalConfig{authinfos, contexts, clusters}
}

func (c CommandOptions) SetClusterName(arn *string) string {
	split := strings.Split(*arn, ":")
	clusterName := strings.TrimPrefix(split[5], "cluster/")
	const breaker = ":"
	switch c.AwsClusterName {
	case false:
		outputName := split[4] + breaker + split[3] + breaker + clusterName
		return outputName
	case true:
		outputName := split[3] + breaker + clusterName
		return outputName
	default:
		return *arn
	}
}

func (c CommandOptions) setCommand() string {
	if c.AwsAuth {
		return "aws-iam-authenticator"
	}
	return "aws"
}

func (c CommandOptions) AwsArgs(region string, clusterName string, arn string) []string {
	var args []string
	if c.AwsRoleString != "" && !c.AwsAuth {
		args = []string{"--region", region, "eks", "get-token", "--cluster-name", clusterName, "--role-arn", "arn:aws:iam::" + SplitAzIDAndGiveItem(arn, ":", 4) + ":role/" + c.AwsRoleString}
	} else if c.AwsRoleString != "" && c.AwsAuth {
		args = []string{"token", "-i", clusterName, "--role-arn", "arn:aws:iam::" + SplitAzIDAndGiveItem(arn, ":", 4) + ":role/" + c.AwsRoleString}
	} else {
		args = []string{"--region", region, "eks", "get-token", "--cluster-name", clusterName}
	}
	return args
}

func (c CommandOptions) AwsEnvs(profile string) interface{} {
	if c.AwsEnvProfile {
		var envArray []Env
		envArray = append(envArray, Env{Name: "AWS_PROFILE", Value: profile})
		return envArray
	}
	return nil
}

func (c CommandOptions) GetSingleAWSCluster(clusterToFind string) Cluster {
	log.Info("Starting AWS find cluster named: " + clusterToFind)
	core.CheckEnvVarOrSitIt("AWS_REGION", c.AwsRegion)
	//get Profiles//search this name in account
	var f Cluster
	profiles := c.GetLocalAwsProfiles()
	c0 := make(chan Cluster)
	// search each profile
	for _, profile := range profiles {
		go c.getAwsClusters(c0, profile, clusterToFind)
	}
	for i := 0; i < len(profiles); i++ {
		res := <-c0
		if res.Name == clusterToFind {
			f = res
		}
	}
	return f
	//search this name in region
	//once it is found erturn info to the user
}

func (c CommandOptions) getAwsClusters(c0 chan Cluster, profile AwsProfiles, clusterToFind string) {
	var re Cluster
	log.Info(string("Using AWS profile: " + profile.Name))
	regions := profile.listRegions()
	profiles := c.GetLocalAwsProfiles()
	addOnVersion := profiles[0].getVersion()
	c2 := make(chan []Cluster)
	for _, reg := range regions {
		go printOutResult(reg, clusterToFind, profile, addOnVersion, c2)
	}
	for i := 0; i < len(regions); i++ {
		aRegion := <-c2
		if len(aRegion) > 0 {
			for _, cluster := range aRegion {
				if cluster.Name == clusterToFind {
					re = cluster
				}
			}
		}
	}
	c0 <- re
}

func checkIfItsAssumeRole(keys []*ini.Key) (bool, string) {
	var ARNRegexp = regexp.MustCompile(`^arn:(\w|-)*:iam::\d+:role\/?(\w+|-|\/|\.)*$`)
	for _, a := range keys {
		if ARNRegexp.MatchString(a.String()) {
			log.Debug("Is ARN: " + a.String())
			return true, a.String()
		}
	}
	return false, ""
}

func stsAssumeRole(awsProfile AwsProfiles) *aws.CredentialsCache {
	roleSession := "default"
	conf, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("us-east-1"),
		config.WithSharedConfigProfile(roleSession))
	core.FailOnError(err, awsErrorMessage)
	appCreds := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(conf), awsProfile.Arn)
	creds := aws.NewCredentialsCache(appCreds)
	log.Debugf("Succsefully triggered stsAssumeRole for %s", awsProfile.Name)
	return creds
}

func XinAwsProfiles(x string, y []AwsProfiles) (int, bool) {
	for t := range y {
		if strings.Contains(y[t].ConfProfile, x) {
			log.Debugf("profile %s is the %x in list", x, t)
			return t, true
		}
	}
	return 0, false
}

func removeString(word, arn string) string {
	if !strings.Contains("default", arn) && strings.Contains(arn, word) {
		log.Debugf("Cutting %s from %s", word, arn)
		split := strings.Split(arn, " ")
		log.Debugf("length is %x and the profile name will be: %s", len(split), split[1])
		return split[1]
	} else if strings.Contains(word, arn) {
		return ""
	} else {
		return arn
	}
}
