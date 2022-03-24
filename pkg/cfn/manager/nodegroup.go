package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go/aws"
	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/blang/semver"
	"github.com/kris-nova/logger"
	"github.com/pkg/errors"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/builder"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
	"github.com/weaveworks/eksctl/pkg/version"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

// NodeGroupStack represents a nodegroup and its type
type NodeGroupStack struct {
	NodeGroupName string
	Type          api.NodeGroupType
	Stack         *Stack
}

// makeNodeGroupStackName generates the name of the nodegroup stack identified by its name, isolated by the cluster this StackCollection operates on
func (c *StackCollection) makeNodeGroupStackName(name string) string {
	return fmt.Sprintf("eksctl-%s-nodegroup-%s", c.spec.Metadata.Name, name)
}

// createNodeGroupTask creates the nodegroup
func (c *StackCollection) createNodeGroupTask(ctx context.Context, errs chan error, ng *api.NodeGroup, forceAddCNIPolicy bool, vpcImporter vpc.Importer) error {
	name := c.makeNodeGroupStackName(ng.Name)

	logger.Info("building nodegroup stack %q", name)
	bootstrapper, err := nodebootstrap.NewBootstrapper(c.spec, ng)
	if err != nil {
		return errors.Wrap(err, "error creating bootstrapper")
	}
	stack := builder.NewNodeGroupResourceSet(c.ec2API, c.iamAPI, c.spec, ng, bootstrapper, forceAddCNIPolicy, vpcImporter)
	if err := stack.AddAllResources(ctx); err != nil {
		return err
	}

	if ng.Tags == nil {
		ng.Tags = make(map[string]string)
	}
	ng.Tags[api.NodeGroupNameTag] = ng.Name
	ng.Tags[api.OldNodeGroupNameTag] = ng.Name
	ng.Tags[api.NodeGroupTypeTag] = string(api.NodeGroupTypeUnmanaged)

	return c.CreateStack(name, stack, ng.Tags, nil, errs)
}

func (c *StackCollection) createManagedNodeGroupTask(ctx context.Context, errorCh chan error, ng *api.ManagedNodeGroup, forceAddCNIPolicy bool, vpcImporter vpc.Importer) error {
	name := c.makeNodeGroupStackName(ng.Name)
	cluster, err := c.DescribeClusterStack()
	if err != nil {
		return err
	}
	if cluster == nil && c.spec.IPv6Enabled() {
		return errors.New("managed nodegroups cannot be created on IPv6 unowned clusters")
	}
	logger.Info("building managed nodegroup stack %q", name)
	bootstrapper := nodebootstrap.NewManagedBootstrapper(c.spec, ng)
	stack := builder.NewManagedNodeGroup(c.ec2API, c.spec, ng, builder.NewLaunchTemplateFetcher(c.ec2API), bootstrapper, forceAddCNIPolicy, vpcImporter)
	if err := stack.AddAllResources(ctx); err != nil {
		return err
	}

	return c.CreateStack(name, stack, ng.Tags, nil, errorCh)
}

func (c *StackCollection) propagateManagedNodeGroupTagsToASGTask(errorCh chan error, ng *api.ManagedNodeGroup) error {
	if ng.DisableASGTagPropagation != nil && *ng.DisableASGTagPropagation {
		return nil
	}

	// describe node group to retrieve ASG names
	input := &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(c.spec.Metadata.Name),
		NodegroupName: aws.String(ng.Name),
	}
	res, err := c.eksAPI.DescribeNodegroup(input)
	if err != nil {
		return errors.Wrapf(err, "couldn't get managed nodegroup details for nodegroup %q", ng.Name)
	}

	// set the managed nodegroup tags to all the ASGs found
	if res.Nodegroup.Resources != nil {
		// build the input tags for all ASGs attached to the managed nodegroup
		asgTags := []*autoscaling.Tag{}

		for _, asg := range res.Nodegroup.Resources.AutoScalingGroups {
			for ngTagKey, ngTagValue := range ng.Tags {
				asgTag := &autoscaling.Tag{
					ResourceId:        aws.String(*asg.Name),
					ResourceType:      aws.String("auto-scaling-group"),
					Key:               aws.String(ngTagKey),
					Value:             aws.String(ngTagValue),
					PropagateAtLaunch: aws.Bool(false),
				}
				asgTags = append(asgTags, asgTag)
			}
		}

		input := &autoscaling.CreateOrUpdateTagsInput{Tags: asgTags}
		if _, err := c.asgAPI.CreateOrUpdateTags(input); err != nil {
			return errors.Wrapf(err, "creating or updating asg tags for managed nodegroup %q", ng.Name)
		}
	}

	go func() { errorCh <- nil }()
	return nil
}

// DescribeNodeGroupStacks calls DescribeStacks and filters out nodegroups
func (c *StackCollection) DescribeNodeGroupStacks() ([]*Stack, error) {
	stacks, err := c.DescribeStacks()
	if err != nil {
		return nil, err
	}

	if len(stacks) == 0 {
		return nil, nil
	}

	nodeGroupStacks := []*Stack{}
	for _, s := range stacks {
		switch *s.StackStatus {
		case cfn.StackStatusDeleteComplete:
			continue
		case cfn.StackStatusDeleteFailed:
			logger.Warning("stack's status of nodegroup named %s is %s", *s.StackName, *s.StackStatus)
			continue
		}
		if c.GetNodeGroupName(s) != "" {
			nodeGroupStacks = append(nodeGroupStacks, s)
		}
	}
	logger.Debug("nodegroups = %v", nodeGroupStacks)
	return nodeGroupStacks, nil
}

// ListNodeGroupStacks returns a list of NodeGroupStacks
func (c *StackCollection) ListNodeGroupStacks() ([]NodeGroupStack, error) {
	stacks, err := c.DescribeNodeGroupStacks()
	if err != nil {
		return nil, err
	}
	var nodeGroupStacks []NodeGroupStack
	for _, stack := range stacks {
		nodeGroupType, err := GetNodeGroupType(stack.Tags)
		if err != nil {
			return nil, err
		}
		nodeGroupStacks = append(nodeGroupStacks, NodeGroupStack{
			NodeGroupName: c.GetNodeGroupName(stack),
			Type:          nodeGroupType,
			Stack:         stack,
		})
	}
	return nodeGroupStacks, nil
}

// DescribeNodeGroupStacksAndResources calls DescribeNodeGroupStacks and fetches all resources,
// then returns it in a map by nodegroup name
func (c *StackCollection) DescribeNodeGroupStacksAndResources() (map[string]StackInfo, error) {
	stacks, err := c.DescribeNodeGroupStacks()
	if err != nil {
		return nil, err
	}

	allResources := make(map[string]StackInfo)

	for _, s := range stacks {
		input := &cfn.DescribeStackResourcesInput{
			StackName: s.StackName,
		}
		resources, err := c.cloudformationAPI.DescribeStackResources(input)
		if err != nil {
			return nil, errors.Wrapf(err, "getting all resources for %q stack", *s.StackName)
		}
		allResources[c.GetNodeGroupName(s)] = StackInfo{
			Resources: resources.StackResources,
			Stack:     s,
		}
	}

	return allResources, nil
}

func (c *StackCollection) GetAutoScalingGroupName(s *Stack) (string, error) {

	nodeGroupType, err := GetNodeGroupType(s.Tags)
	if err != nil {
		return "", err
	}

	switch nodeGroupType {
	case api.NodeGroupTypeManaged:
		res, err := c.getManagedNodeGroupAutoScalingGroupName(s)
		if err != nil {
			return "", err
		}
		return res, nil
	case api.NodeGroupTypeUnmanaged, "":
		res, err := c.GetUnmanagedNodeGroupAutoScalingGroupName(s)
		if err != nil {
			return "", err
		}
		return res, nil

	default:
		return "", fmt.Errorf("cant get autoscaling group name, because unexpected nodegroup type : %q", nodeGroupType)
	}
}

// GetNodeGroupAutoScalingGroupName returns the unmanaged nodegroup's AutoScalingGroupName
func (c *StackCollection) GetUnmanagedNodeGroupAutoScalingGroupName(s *Stack) (string, error) {
	input := &cfn.DescribeStackResourceInput{
		StackName:         s.StackName,
		LogicalResourceId: aws.String("NodeGroup"),
	}

	res, err := c.cloudformationAPI.DescribeStackResource(input)
	if err != nil {
		return "", err
	}
	return *res.StackResourceDetail.PhysicalResourceId, nil
}

// GetManagedNodeGroupAutoScalingGroupName returns the managed nodegroup's AutoScalingGroup names
func (c *StackCollection) getManagedNodeGroupAutoScalingGroupName(s *Stack) (string, error) {
	input := &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(getClusterNameTag(s)),
		NodegroupName: aws.String(c.GetNodeGroupName(s)),
	}

	res, err := c.eksAPI.DescribeNodegroup(input)
	if err != nil {
		logger.Warning("couldn't get managed nodegroup details for stack %q", *s.StackName)
		return "", nil
	}

	var asgs []string

	if res.Nodegroup.Resources != nil {
		for _, v := range res.Nodegroup.Resources.AutoScalingGroups {
			asgs = append(asgs, aws.StringValue(v.Name))
		}
	}
	return strings.Join(asgs, ","), nil
}

// GetAutoScalingGroupDesiredCapacity returns the AutoScalingGroup's desired capacity
func (c *StackCollection) GetAutoScalingGroupDesiredCapacity(ctx context.Context, name string) (types.AutoScalingGroup, error) {
	asg, err := c.asgAPI.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{
			name,
		},
	})

	if err != nil {
		return types.AutoScalingGroup{}, fmt.Errorf("couldn't describe ASG: %s", name)
	}
	if len(asg.AutoScalingGroups) != 1 {
		logger.Warning("couldn't find ASG %s", name)
		return types.AutoScalingGroup{}, fmt.Errorf("couldn't find ASG: %s", name)
	}

	return asg.AutoScalingGroups[0], nil
}

// DescribeNodeGroupStack gets the specified nodegroup stack
func (c *StackCollection) DescribeNodeGroupStack(nodeGroupName string) (*Stack, error) {
	stackName := c.makeNodeGroupStackName(nodeGroupName)
	return c.DescribeStack(&Stack{StackName: &stackName})
}

// GetNodeGroupStackType returns the nodegroup stack type
func (c *StackCollection) GetNodeGroupStackType(options GetNodegroupOption) (api.NodeGroupType, error) {
	var (
		err   error
		stack *Stack
	)
	if options.Stack != nil && options.Stack.Stack != nil {
		stack = options.Stack.Stack
	}
	if stack == nil {
		stack, err = c.DescribeNodeGroupStack(options.NodeGroupName)
		if err != nil {
			return "", err
		}
	}
	return GetNodeGroupType(stack.Tags)
}

// GetNodeGroupType returns the nodegroup type
func GetNodeGroupType(tags []*cfn.Tag) (api.NodeGroupType, error) {
	var nodeGroupType api.NodeGroupType

	if ngNameTagValue := GetNodegroupTagName(tags); ngNameTagValue == "" {
		return "", errors.New("failed to find the nodegroup name tag")
	}

	for _, tag := range tags {
		switch *tag.Key {
		case api.NodeGroupTypeTag:
			nodeGroupType = api.NodeGroupType(*tag.Value)
		}
	}

	if nodeGroupType == "" {
		nodeGroupType = api.NodeGroupTypeUnmanaged
	}

	return nodeGroupType, nil
}

// GetEksctlVersionFromTags returns the eksctl version used to create or update the stack
func GetEksctlVersionFromTags(tags []*cfn.Tag) (semver.Version, bool, error) {
	for _, tag := range tags {
		if *tag.Key == api.EksctlVersionTag {
			v, err := version.ParseEksctlVersion(*tag.Value)
			if err != nil {
				return v, false, errors.Wrapf(err, "unexpected error parsing eksctl version %q", *tag.Value)
			}
			return v, true, nil
		}
	}
	return semver.Version{}, false, nil
}

// GetNodeGroupName will return nodegroup name based on tags
func (*StackCollection) GetNodeGroupName(s *Stack) string {
	if tagName := GetNodegroupTagName(s.Tags); tagName != "" {
		return tagName
	}
	if strings.HasSuffix(*s.StackName, "-nodegroup-0") {
		return "legacy-nodegroup-0"
	}
	if strings.HasSuffix(*s.StackName, "-DefaultNodeGroup") {
		return "legacy-default"
	}
	return ""
}

// GetNodegroupTagName returns the nodegroup name of a stack based on its tags. Taking into account legacy tags.
func GetNodegroupTagName(tags []*cfn.Tag) string {
	for _, tag := range tags {
		switch *tag.Key {
		case api.NodeGroupNameTag, api.OldNodeGroupNameTag, api.OldNodeGroupIDTag:
			return *tag.Value
		}
	}
	return ""
}
