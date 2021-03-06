package discovery

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/haproxytech/client-native/v2/configuration"
	"github.com/haproxytech/client-native/v2/models"

	"github.com/haproxytech/dataplaneapi/haproxy"
)

const (
	HAProxyServiceNameTag  = "HAProxy:Service:Name"
	HAProxyServicePortTag  = "HAProxy:Service:Port"
	HAProxyInstancePortTag = "HAProxy:Instance:Port"
)

type awsInstance struct {
	params          *models.AwsRegion
	timeout         time.Duration
	ctx             context.Context
	update          chan struct{}
	cancel          context.CancelFunc
	state           map[string]map[string]time.Time
	discoveryConfig *ServiceDiscoveryInstance
}

type awsService struct {
	name                       string
	region, instanceName, ipv4 string
	instances                  map[string]types.Instance
	changed                    bool
}

func (a awsService) GetName() string {
	return a.name
}

func (a awsService) GetBackendName() string {
	return fmt.Sprintf("aws-%s-%s-%s", a.region, a.instanceName, a.GetName())
}

func (a awsService) Changed() bool {
	return a.changed
}

func (a awsService) GetServers() (servers []configuration.ServiceServer) {
	for _, instance := range a.instances {
		port, _ := a.instancePortFromEC2(instance)
		var address string
		switch a.ipv4 {
		case models.AwsRegionIPV4AddressPrivate:
			address = aws.ToString(instance.PrivateIpAddress)
		case models.AwsRegionIPV4AddressPublic:
			address = aws.ToString(instance.PublicIpAddress)
		default:
			continue
		}
		// In case of public IPv4 and the instance doesn't have it, ignoring.
		if len(address) == 0 {
			continue
		}
		servers = append(servers, configuration.ServiceServer{
			Address: address,
			Port:    port,
		})
	}
	return
}

func newAWSRegionInstance(params *models.AwsRegion, client *configuration.Client, reloadAgent haproxy.IReloadAgent) (*awsInstance, error) {
	timeout, err := time.ParseDuration(fmt.Sprintf("%ds", *params.RetryTimeout))
	if err != nil {
		return nil, err
	}

	ai := &awsInstance{
		params:  params,
		timeout: timeout,
		update:  make(chan struct{}),
		state:   make(map[string]map[string]time.Time),
		discoveryConfig: NewServiceDiscoveryInstance(client, reloadAgent, discoveryInstanceParams{
			Whitelist:       []string{},
			Blacklist:       []string{},
			ServerSlotsBase: int(*params.ServerSlotsBase),
			SlotsGrowthType: *params.ServerSlotsGrowthType,
			SlotsIncrement:  int(params.ServerSlotsGrowthIncrement),
		}),
	}
	if err = ai.updateTimeout(*params.RetryTimeout); err != nil {
		return nil, err
	}

	return ai, nil
}

func (a *awsInstance) filterConverter(in []*models.AwsFilters) (out []types.Filter) {
	out = make([]types.Filter, len(in))
	for i, l := range in {
		filter := l
		out[i] = types.Filter{
			Name:   filter.Key,
			Values: []string{aws.ToString(filter.Value)},
		}
	}
	return
}

func (a *awsInstance) updateTimeout(timeoutSeconds int64) error {
	timeout, err := time.ParseDuration(fmt.Sprintf("%ds", timeoutSeconds))
	if err != nil {
		return err
	}
	a.timeout = timeout
	return nil
}

func (a *awsInstance) start() {
	go func() {
		a.ctx, a.cancel = context.WithCancel(context.Background())

		for {
			select {
			case <-a.update:
				err := a.discoveryConfig.UpdateParams(discoveryInstanceParams{
					Whitelist:       []string{},
					Blacklist:       []string{},
					ServerSlotsBase: int(*a.params.ServerSlotsBase),
					SlotsGrowthType: *a.params.ServerSlotsGrowthType,
					SlotsIncrement:  int(a.params.ServerSlotsGrowthIncrement),
				})
				if err != nil {
					a.stop()
				}
			case <-time.After(a.timeout):
				var api *ec2.Client
				var err error

				if api, err = a.setAPIClient(); err != nil {
					a.stop()
				}
				if err = a.updateServices(api); err != nil {
					a.stop()
				}
			case <-a.ctx.Done():
				return
			}
		}
	}()
}

func (a *awsInstance) setAPIClient() (*ec2.Client, error) {
	opts := []func(options *config.LoadOptions) error{
		config.WithRegion(*a.params.Region),
	}
	if len(a.params.AccessKeyID) > 0 && len(a.params.SecretAccessKey) > 0 {
		opts = append(opts, config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID:     a.params.AccessKeyID,
				SecretAccessKey: a.params.SecretAccessKey,
			},
		}))
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("cannot generate the AWS instance due to a configuration setup error: %w", err)
	}

	return ec2.NewFromConfig(cfg), nil
}

func (a *awsInstance) updateServices(api *ec2.Client) (err error) {
	var io *ec2.DescribeInstancesOutput

	io, err = api.DescribeInstances(a.ctx, &ec2.DescribeInstancesInput{
		Filters: append([]types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{HAProxyServiceNameTag, HAProxyServicePortTag},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running"},
			},
		}, a.filterConverter(a.params.Allowlist)...),
	})
	if err != nil {
		return
	}

	mapService := make(map[string]*awsService)

	for _, r := range io.Reservations {
		for _, i := range r.Instances {
			var sn string
			sn, err = a.serviceNameFromEC2(i)
			if err != nil {
				continue
			}
			// creating empty service in case it isn't there
			if _, ok := mapService[sn]; !ok {
				mapService[sn] = &awsService{
					name:         sn,
					region:       *a.params.Region,
					instanceName: *a.params.Name,
					ipv4:         *a.params.IPV4Address,
					instances:    make(map[string]types.Instance),
				}
			}
			instanceID := aws.ToString(i.InstanceId)
			mapService[sn].instances[instanceID] = i
		}
	}

	if len(a.params.Denylist) > 0 {
		// AWS API doesn't provide negative filter search, so doing on our own
		io, err = api.DescribeInstances(a.ctx, &ec2.DescribeInstancesInput{
			Filters: a.filterConverter(a.params.Denylist),
		})
		if err == nil {
			for _, r := range io.Reservations {
				for _, i := range r.Instances {
					var sn string
					sn, err = a.serviceNameFromEC2(i)
					// definitely we can skip, there's no Service metadata tag
					if err != nil {
						continue
					}
					// neither tracked as Service, we can skip
					if _, ok := mapService[sn]; !ok {
						continue
					}
					// we have an occurrence, we have to delete
					instanceID := aws.ToString(i.InstanceId)
					delete(mapService[sn].instances, instanceID)
				}
			}
		}
	}

	var services []ServiceInstance
	for _, s := range mapService {
		// We don't have a proper way to understand if a Service has changed, or not, this can be achieved
		// iterating over the instances being part of the Service and check the last launch time:
		// if something differs, a change occurred.
		s.changed = func() bool {
			if len(a.state[s.name]) != len(s.instances) {
				return true
			}
			for _, instance := range s.instances {
				instanceID := aws.ToString(instance.InstanceId)
				v, ok := a.state[s.name][instanceID]
				if !ok {
					return true
				}
				if v != *instance.LaunchTime {
					return true
				}
			}
			return false
		}()
		services = append(services, s)

		a.state[s.name] = func(instances map[string]types.Instance) (hash map[string]time.Time) {
			hash = make(map[string]time.Time)
			for _, instance := range instances {
				id := aws.ToString(instance.InstanceId)
				hash[id] = aws.ToTime(instance.LaunchTime)
			}
			return
		}(s.instances)
	}

	return a.discoveryConfig.UpdateServices(services)
}

func (a *awsInstance) stop() {
	a.cancel()
}

func (a *awsService) instancePortFromEC2(instance types.Instance) (port int, err error) {
	for _, t := range instance.Tags {
		switch {
		case *t.Key == HAProxyServicePortTag:
			port, err = strconv.Atoi(*t.Value)
		case *t.Key == HAProxyInstancePortTag:
			return strconv.Atoi(*t.Value)
		}
	}
	return
}

func (a *awsInstance) serviceNameFromEC2(instance types.Instance) (string, error) {
	var name, port string
	for _, t := range instance.Tags {
		switch {
		case *t.Key == HAProxyServiceNameTag:
			name = aws.ToString(t.Value)
		case *t.Key == HAProxyServicePortTag:
			port = aws.ToString(t.Value)
		case len(name) > 0 && len(port) > 0:
			break
		}
	}

	if len(name) == 0 || len(port) == 0 {
		return "", fmt.Errorf("missing metadata for instance %s", *instance.InstanceId)
	}

	return fmt.Sprintf("%s-%s", name, port), nil
}
