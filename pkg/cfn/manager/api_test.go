package manager

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/awstesting"
	cfn "github.com/aws/aws-sdk-go/service/cloudformation"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/builder"
	"github.com/weaveworks/eksctl/pkg/testutils/mockprovider"
)

var _ = Describe("StackCollection", func() {
	Context("PropagateManagedNodeGroupTagsToASG", func() {
		It("can create propagate tag", func() {
			// define most mock parameters
			asgName := "asg-test-name"
			ngName := "ng-test-name"
			ngTags := map[string]string{
				"tag_key_1": "tag_value_1",
			}
			errCh := make(chan error)

			p := mockprovider.NewMockProvider()

			// DescribeTags classic mock
			describeTagsInput := &autoscaling.DescribeTagsInput{
				Filters: []astypes.Filter{{Name: aws.String("auto-scaling-group"), Values: []string{asgName}}},
			}
			describeOutput := &autoscaling.DescribeTagsOutput{}
			p.MockASG().On("DescribeTags", mock.Anything, describeTagsInput).Return(describeOutput, nil)

			// CreateOrUpdateTags classic mock
			createOrUpdateTagsInput := &autoscaling.CreateOrUpdateTagsInput{
				Tags: []astypes.Tag{
					{
						ResourceId:        aws.String("asg-test-name"),
						ResourceType:      aws.String("auto-scaling-group"),
						Key:               aws.String("tag_key_1"),
						Value:             aws.String("tag_value_1"),
						PropagateAtLaunch: aws.Bool(false),
					},
				},
			}
			createOrUpdateTagsOutput := &autoscaling.CreateOrUpdateTagsOutput{}
			p.MockASG().On("CreateOrUpdateTags", mock.Anything, createOrUpdateTagsInput).Return(createOrUpdateTagsOutput, nil)

			sm := NewStackCollection(p, api.NewClusterConfig())
			err := sm.PropagateManagedNodeGroupTagsToASG(ngName, ngTags, []string{asgName}, errCh)
			Expect(err).NotTo(HaveOccurred())
			err = <-errCh
			Expect(err).NotTo(HaveOccurred())
		})
		It("cannot propagate tags in chunks of 25", func() {
			// define most mock parameters
			ngName := "ng-test-name"
			asgName := "asg-test-name"
			ngTags := make(map[string]string)
			// populate the createOrUpdateTagsSliceInput for easier generation of chunks
			createOrUpdateTagsSliceInput := []astypes.Tag{}
			for i := 0; i < 30; i++ {
				tagKey, tagValue := fmt.Sprintf("tag_key_%d", i), fmt.Sprintf("tag_value_%d", i)
				ngTags[tagKey] = tagValue
				createOrUpdateTagsSliceInput = append(createOrUpdateTagsSliceInput, astypes.Tag{
					ResourceId:        aws.String(asgName),
					ResourceType:      aws.String("auto-scaling-group"),
					Key:               aws.String(tagKey),
					Value:             aws.String(tagValue),
					PropagateAtLaunch: aws.Bool(false),
				})
			}
			errCh := make(chan error)

			p := mockprovider.NewMockProvider()

			// DescribeTags classic mock
			describeTagsInput := &autoscaling.DescribeTagsInput{
				Filters: []astypes.Filter{{Name: aws.String("auto-scaling-group"), Values: []string{asgName}}},
			}
			describeOutput := &autoscaling.DescribeTagsOutput{}
			p.MockASG().On("DescribeTags", mock.Anything, describeTagsInput).Return(describeOutput, nil)

			// CreateOrUpdateTags chunked mock
			// generate the expected chunk of tags
			chunkSize := builder.MaximumCreatedTagNumberPerCall
			firstchunkLenMatcher := func(input *autoscaling.CreateOrUpdateTagsInput) bool {
				return len(input.Tags) == len(createOrUpdateTagsSliceInput[:chunkSize])
			}
			secondChunkLenMatcher := func(input *autoscaling.CreateOrUpdateTagsInput) bool {
				return len(input.Tags) == len(createOrUpdateTagsSliceInput[chunkSize:])
			}

			// setup the call verification of the two chunks
			// NOTE: because of the use of map (unordered processing), we just verify size of chunk
			p.MockASG().On("CreateOrUpdateTags", mock.Anything, mock.MatchedBy(firstchunkLenMatcher)).Return(&autoscaling.CreateOrUpdateTagsOutput{}, nil)
			p.MockASG().On("CreateOrUpdateTags", mock.Anything, mock.MatchedBy(secondChunkLenMatcher)).Return(&autoscaling.CreateOrUpdateTagsOutput{}, nil)

			sm := NewStackCollection(p, api.NewClusterConfig())
			err := sm.PropagateManagedNodeGroupTagsToASG(ngName, ngTags, []string{asgName}, errCh)
			Expect(err).NotTo(HaveOccurred())
			err = <-errCh
			Expect(err).NotTo(HaveOccurred())
		})
		It("cannot propagate if to many tags", func() {
			// define parameters
			ngName := "ng-test-name"
			asgName := "asg-test-name"
			ngTags := make(map[string]string)
			for i := 0; i < builder.MaximumTagNumber+1; i++ {
				ngTags[fmt.Sprintf("tag_key_%d", i)] = fmt.Sprintf("tag_value_%d", i)
			}
			errCh := make(chan error)

			p := mockprovider.NewMockProvider()

			// DescribeTags classic mock
			describeTagsInput := &autoscaling.DescribeTagsInput{
				Filters: []astypes.Filter{{Name: aws.String("auto-scaling-group"), Values: []string{asgName}}},
			}
			describeOutput := &autoscaling.DescribeTagsOutput{}
			p.MockASG().On("DescribeTags", mock.Anything, describeTagsInput).Return(describeOutput, nil)

			sm := NewStackCollection(p, api.NewClusterConfig())
			err := sm.PropagateManagedNodeGroupTagsToASG(ngName, ngTags, []string{asgName}, errCh)
			Expect(err).NotTo(HaveOccurred())
			err = <-errCh
			Expect(err).To(MatchError(ContainSubstring("maximum amount for asg")))
		})
	})

	Context("UpdateStack", func() {
		It("succeeds if no changes required", func() {
			// Order of AWS SDK invocation
			// 1) DescribeStacks
			// 2) CreateChangeSet
			// 3) DescribeChangeSetRequest (FAILED to abort early)
			// 4) DescribeChangeSet (StatusReason contains "The submitted information didn't contain changes" to exit with noChangeError)

			stackName := "eksctl-stack"
			changeSetName := "eksctl-changeset"
			describeInput := &cfn.DescribeStacksInput{StackName: &stackName}
			describeOutput := &cfn.DescribeStacksOutput{Stacks: []*cfn.Stack{{
				StackName:   &stackName,
				StackStatus: aws.String(cfn.StackStatusCreateComplete),
			}}}
			describeChangeSetFailed := &cfn.DescribeChangeSetOutput{
				StackName:     &stackName,
				ChangeSetName: &changeSetName,
				Status:        aws.String(cfn.ChangeSetStatusFailed),
			}
			describeChangeSetNoChange := &cfn.DescribeChangeSetOutput{
				StackName:    &stackName,
				StatusReason: aws.String("The submitted information didn't contain changes"),
			}
			p := mockprovider.NewMockProvider()
			p.MockCloudFormation().On("DescribeStacks", describeInput).Return(describeOutput, nil)
			p.MockCloudFormation().On("CreateChangeSet", mock.Anything).Return(nil, nil)
			req := awstesting.NewClient(nil).NewRequest(&request.Operation{Name: "Operation"}, nil, describeChangeSetFailed)
			p.MockCloudFormation().On("DescribeChangeSetRequest", mock.Anything).Return(req, describeChangeSetFailed)
			p.MockCloudFormation().On("DescribeChangeSet", mock.Anything).Return(describeChangeSetNoChange, nil)

			sm := NewStackCollection(p, api.NewClusterConfig())
			err := sm.UpdateStack(UpdateStackOptions{
				StackName:     stackName,
				ChangeSetName: changeSetName,
				Description:   "description",
				TemplateData:  TemplateBody(""),
				Wait:          true,
			})
			Expect(err).NotTo(HaveOccurred())
		})
		It("can update when only the stack is provided", func() {
			// Order of AWS SDK invocation
			// 1) DescribeStacks
			// 2) CreateChangeSet
			// 3) DescribeChangeSetRequest (FAILED to abort early)
			// 4) DescribeChangeSet (StatusReason contains "The submitted information didn't contain changes" to exit with noChangeError)

			stackName := "eksctl-stack"
			changeSetName := "eksctl-changeset"
			describeInput := &cfn.DescribeStacksInput{StackName: &stackName}
			describeOutput := &cfn.DescribeStacksOutput{Stacks: []*cfn.Stack{{
				StackName:   &stackName,
				StackStatus: aws.String(cfn.StackStatusCreateComplete),
			}}}
			describeChangeSetFailed := &cfn.DescribeChangeSetOutput{
				StackName:     &stackName,
				ChangeSetName: &changeSetName,
				Status:        aws.String(cfn.ChangeSetStatusFailed),
			}
			describeChangeSetNoChange := &cfn.DescribeChangeSetOutput{
				StackName:    &stackName,
				StatusReason: aws.String("The submitted information didn't contain changes"),
			}
			p := mockprovider.NewMockProvider()
			p.MockCloudFormation().On("DescribeStacks", describeInput).Return(describeOutput, nil)
			p.MockCloudFormation().On("CreateChangeSet", mock.Anything).Return(nil, nil)
			req := awstesting.NewClient(nil).NewRequest(&request.Operation{Name: "Operation"}, nil, describeChangeSetFailed)
			p.MockCloudFormation().On("DescribeChangeSetRequest", mock.Anything).Return(req, describeChangeSetFailed)
			p.MockCloudFormation().On("DescribeChangeSet", mock.Anything).Return(describeChangeSetNoChange, nil)

			sm := NewStackCollection(p, api.NewClusterConfig())
			err := sm.UpdateStack(UpdateStackOptions{
				Stack: &Stack{
					StackName: &stackName,
				},
				ChangeSetName: changeSetName,
				Description:   "description",
				TemplateData:  TemplateBody(""),
				Wait:          true,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	It("updates tags (existing + metadata + auto)", func() {
		// Order of execution
		// 1) DescribeStacks
		// 2) CreateChangeSet
		// 3) DescribeChangeSetRequest (until CREATE_COMPLETE)
		// 4) DescribeChangeSet
		// 5) ExecuteChangeSet
		// 6) DescribeStacksRequest (until UPDATE_COMPLETE)

		clusterName := "clusteur"
		stackName := "eksctl-stack"
		changeSetName := "eksctl-changeset"
		describeInput := &cfn.DescribeStacksInput{StackName: &stackName}
		existingTag := &cfn.Tag{
			Key:   aws.String("existing"),
			Value: aws.String("tag"),
		}
		describeOutput := &cfn.DescribeStacksOutput{Stacks: []*cfn.Stack{{
			StackName:   &stackName,
			StackStatus: aws.String(cfn.StackStatusCreateComplete),
			Tags:        []*cfn.Tag{existingTag},
		}}}
		describeChangeSetCreateCompleteOutput := &cfn.DescribeChangeSetOutput{
			StackName:     &stackName,
			ChangeSetName: &changeSetName,
			Status:        aws.String(cfn.ChangeSetStatusCreateComplete),
		}
		describeStacksUpdateCompleteOutput := &cfn.DescribeStacksOutput{
			Stacks: []*cfn.Stack{
				{
					StackName:   &stackName,
					StackStatus: aws.String(cfn.StackStatusUpdateComplete),
				},
			},
		}
		executeChangeSetInput := &cfn.ExecuteChangeSetInput{
			ChangeSetName: &changeSetName,
			StackName:     &stackName,
		}

		p := mockprovider.NewMockProvider()
		p.MockCloudFormation().On("DescribeStacks", describeInput).Return(describeOutput, nil)
		p.MockCloudFormation().On("CreateChangeSet", mock.Anything).Return(nil, nil)
		req := awstesting.NewClient(nil).NewRequest(&request.Operation{Name: "Operation"}, nil, describeChangeSetCreateCompleteOutput)
		p.MockCloudFormation().On("DescribeChangeSetRequest", mock.Anything).Return(req, describeChangeSetCreateCompleteOutput)
		p.MockCloudFormation().On("DescribeChangeSet", mock.Anything).Return(describeChangeSetCreateCompleteOutput, nil)
		p.MockCloudFormation().On("ExecuteChangeSet", executeChangeSetInput).Return(nil, nil)
		req = awstesting.NewClient(nil).NewRequest(&request.Operation{Name: "Operation"}, nil, describeStacksUpdateCompleteOutput)
		p.MockCloudFormation().On("DescribeStacksRequest", mock.Anything).Return(req, describeStacksUpdateCompleteOutput)

		spec := api.NewClusterConfig()
		spec.Metadata.Name = clusterName
		spec.Metadata.Tags = map[string]string{"meta": "data"}
		sm := NewStackCollection(p, spec)
		err := sm.UpdateStack(UpdateStackOptions{
			StackName:     stackName,
			ChangeSetName: changeSetName,
			Description:   "description",
			TemplateData:  TemplateBody(""),
			Wait:          true,
		})
		Expect(err).NotTo(HaveOccurred())

		// Second is CreateChangeSet() call which we are interested in
		args := p.MockCloudFormation().Calls[1].Arguments.Get(0)
		createChangeSetInput := args.(*cfn.CreateChangeSetInput)
		// Existing tag
		Expect(createChangeSetInput.Tags).To(ContainElement(existingTag))
		// Auto-populated tag
		Expect(createChangeSetInput.Tags).To(ContainElement(&cfn.Tag{Key: aws.String(api.ClusterNameTag), Value: &clusterName}))
		// Metadata tag
		Expect(createChangeSetInput.Tags).To(ContainElement(&cfn.Tag{Key: aws.String("meta"), Value: aws.String("data")}))
	})
	When("wait is set to false", func() {
		It("will skip the last wait sequence", func() {
			clusterName := "cluster"
			stackName := "eksctl-stack"
			changeSetName := "eksctl-changeset"
			describeInput := &cfn.DescribeStacksInput{StackName: &stackName}
			existingTag := &cfn.Tag{
				Key:   aws.String("existing"),
				Value: aws.String("tag"),
			}
			describeOutput := &cfn.DescribeStacksOutput{Stacks: []*cfn.Stack{{
				StackName:   &stackName,
				StackStatus: aws.String(cfn.StackStatusCreateComplete),
				Tags:        []*cfn.Tag{existingTag},
			}}}
			describeChangeSetCreateCompleteOutput := &cfn.DescribeChangeSetOutput{
				StackName:     &stackName,
				ChangeSetName: &changeSetName,
				Status:        aws.String(cfn.ChangeSetStatusCreateComplete),
			}
			executeChangeSetInput := &cfn.ExecuteChangeSetInput{
				ChangeSetName: &changeSetName,
				StackName:     &stackName,
			}

			p := mockprovider.NewMockProvider()
			p.MockCloudFormation().On("DescribeStacks", describeInput).Return(describeOutput, nil)
			p.MockCloudFormation().On("CreateChangeSet", mock.Anything).Return(nil, nil)
			req := awstesting.NewClient(nil).NewRequest(&request.Operation{Name: "Operation"}, nil, describeChangeSetCreateCompleteOutput)
			p.MockCloudFormation().On("DescribeChangeSetRequest", mock.Anything).Return(req, describeChangeSetCreateCompleteOutput)
			p.MockCloudFormation().On("DescribeChangeSet", mock.Anything).Return(describeChangeSetCreateCompleteOutput, nil)
			p.MockCloudFormation().On("ExecuteChangeSet", executeChangeSetInput).Return(nil, nil)
			// For the future, this is the call we do not expect to happen, and this is the difference compared to the
			// above test case.
			// p.MockCloudFormation().On("DescribeStacksRequest", mock.Anything).Return(req, describeStacksUpdateCompleteOutput)

			spec := api.NewClusterConfig()
			spec.Metadata.Name = clusterName
			spec.Metadata.Tags = map[string]string{"meta": "data"}
			sm := NewStackCollection(p, spec)
			err := sm.UpdateStack(UpdateStackOptions{
				StackName:     stackName,
				ChangeSetName: changeSetName,
				Description:   "description",
				TemplateData:  TemplateBody(""),
				Wait:          false,
			})
			Expect(err).NotTo(HaveOccurred())

			// Second is CreateChangeSet() call which we are interested in
			args := p.MockCloudFormation().Calls[1].Arguments.Get(0)
			createChangeSetInput := args.(*cfn.CreateChangeSetInput)
			// Existing tag
			Expect(createChangeSetInput.Tags).To(ContainElement(existingTag))
			// Auto-populated tag
			Expect(createChangeSetInput.Tags).To(ContainElement(&cfn.Tag{Key: aws.String(api.ClusterNameTag), Value: &clusterName}))
			// Metadata tag
			Expect(createChangeSetInput.Tags).To(ContainElement(&cfn.Tag{Key: aws.String("meta"), Value: aws.String("data")}))
		})
	})

	Context("HasClusterStackFromList", func() {
		type clusterInput struct {
			clusterName   string
			eksctlCreated bool
		}

		DescribeTable("should work for eksctl-created clusters", func(ci clusterInput) {
			clusterConfig := api.NewClusterConfig()
			clusterConfig.Metadata.Name = ci.clusterName
			stackName := aws.String(fmt.Sprintf("eksctl-%s-cluster", clusterConfig.Metadata.Name))

			var out *cfn.DescribeStacksOutput
			if ci.eksctlCreated {
				out = &cfn.DescribeStacksOutput{
					Stacks: []*cfn.Stack{
						{
							StackName: stackName,
							Tags: []*cfn.Tag{
								{
									Key:   aws.String("alpha.eksctl.io/cluster-name"),
									Value: aws.String(clusterConfig.Metadata.Name),
								},
							},
						},
					},
				}
			} else {
				out = &cfn.DescribeStacksOutput{}
			}

			p := mockprovider.NewMockProvider()
			p.MockCloudFormation().On("DescribeStacks", &cfn.DescribeStacksInput{StackName: stackName}).Return(out, nil)

			s := NewStackCollection(p, clusterConfig)
			hasClusterStack, err := s.HasClusterStackFromList([]string{
				"eksctl-test-cluster",
				*stackName,
			}, clusterConfig.Metadata.Name)

			if ci.eksctlCreated {
				Expect(err).NotTo(HaveOccurred())
				Expect(hasClusterStack).To(Equal(true))
			} else {
				Expect(err).To(MatchError(fmt.Sprintf("no CloudFormation stack found for %s", *stackName)))
			}
		},
			Entry("cluster stack exists", clusterInput{
				clusterName:   "web",
				eksctlCreated: true,
			}),
			Entry("cluster stack does not exist", clusterInput{
				clusterName:   "unowned",
				eksctlCreated: false,
			}),
		)
	})

	Context("GetClusterStackIfExists", func() {
		var (
			cfg                 *api.ClusterConfig
			p                   *mockprovider.MockProvider
			stackNameWithEksctl string
		)
		BeforeEach(func() {
			stackName := "confirm-this"
			stackNameWithEksctl = "eksctl-" + stackName + "-cluster"
			describeInput := &cfn.DescribeStacksInput{StackName: &stackNameWithEksctl}
			describeOutput := &cfn.DescribeStacksOutput{Stacks: []*cfn.Stack{{
				StackName:   &stackName,
				StackStatus: aws.String(cfn.StackStatusCreateComplete),
				Tags: []*cfn.Tag{
					{
						Key:   aws.String(api.ClusterNameTag),
						Value: &stackName,
					},
				},
			}}}
			p = mockprovider.NewMockProvider()
			p.MockCloudFormation().On("DescribeStacks", describeInput).Return(describeOutput, nil)

			cfg = api.NewClusterConfig()
			cfg.Metadata.Name = stackName
		})

		It("can retrieve stacks", func() {
			p.MockCloudFormation().On("ListStacksPages", mock.Anything, mock.AnythingOfType("func(*cloudformation.ListStacksOutput, bool) bool")).Run(func(args mock.Arguments) {
				fn := args.Get(1) // the passed in function
				fn.(func(p *cfn.ListStacksOutput, _ bool) bool)(&cfn.ListStacksOutput{
					StackSummaries: []*cfn.StackSummary{
						{
							StackName: &stackNameWithEksctl,
						},
					},
				}, true)
			}).Return(nil)
			sm := NewStackCollection(p, cfg)
			stack, err := sm.GetClusterStackIfExists()
			Expect(err).NotTo(HaveOccurred())
			Expect(stack).NotTo(BeNil())
		})

		When("the config stack doesn't match", func() {
			It("returns no stack", func() {
				p.MockCloudFormation().On("ListStacksPages", mock.Anything, mock.AnythingOfType("func(*cloudformation.ListStacksOutput, bool) bool")).Run(func(args mock.Arguments) {
					fn := args.Get(1) // the passed in function
					fn.(func(p *cfn.ListStacksOutput, _ bool) bool)(&cfn.ListStacksOutput{
						StackSummaries: []*cfn.StackSummary{
							{
								StackName: &stackNameWithEksctl,
							},
						},
					}, true)
				}).Return(nil)
				cfg.Metadata.Name = "not-this"
				sm := NewStackCollection(p, cfg)
				stack, err := sm.GetClusterStackIfExists()
				Expect(err).NotTo(HaveOccurred())
				Expect(stack).To(BeNil())
			})
		})

		When("ListStacksPages errors", func() {
			It("errors", func() {
				p.MockCloudFormation().On("ListStacksPages", mock.Anything, mock.AnythingOfType("func(*cloudformation.ListStacksOutput, bool) bool")).Return(errors.New("nope"))
				sm := NewStackCollection(p, cfg)
				_, err := sm.GetClusterStackIfExists()
				Expect(err).To(MatchError(ContainSubstring("nope")))
			})
		})
	})
})
