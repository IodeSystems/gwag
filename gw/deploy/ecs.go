package deploy

import (
	"encoding/json"
	"fmt"
)

// ECS on Fargate, one ECS service per node (desired count 1):
//
//   - Stable peer DNS: each service registers in a Cloud Map private
//     namespace as gwag-<i>.<namespace>; peers dial that name.
//   - Volume: EFS, one access point per node. ECS-managed EBS volumes
//     are deleted with the task and are not reattached to its
//     replacement, so the node would come back with an empty data dir
//     and a new node ID. EFS survives task replacement and moves
//     across AZs. Cost: JetStream fsyncs over NFS. Not measured.
//   - At most one node down: every service runs min healthy 0 / max 100
//     (the old task stops before the new one starts, so two tasks never
//     share one EFS directory or node name). Updates must also not hit
//     two services at once. A CloudFormation DependsOn chain would do
//     it but deadlocks a first create: service 1 waits for a healthy
//     target, which needs service 2. So each node has its own image
//     parameter (Image1..ImageN); change one per stack update.
//   - ALB target group checks /api/health (200 only). Health check grace
//     keeps a starting node (503 until it has a meta leader) alive.
//   - Token: Secrets Manager generates a 64-char hex secret; tasks read
//     it as GWAG_ADMIN_TOKEN.
//
// The template is JSON (valid CloudFormation) built from Go maps, so
// output is deterministic and needs no quoting care.

type m = map[string]any
type l = []any

// ecsRefs names the account-specific values of a container definition.
// In the template they are intrinsics; in the standalone task
// definition they are REPLACE_* placeholders.
type ecsRefs struct {
	secretARN, fileSystem, accessPoint, execRole, logGroup any
	image, region                                          any
}

func ecsCommand(s Spec, i int, nsSuffix func(j int) any) l {
	var out l
	for _, a := range nodeArgs(fmt.Sprintf("gwag-%d", i), nil) {
		out = append(out, a)
	}
	if s.Replicas > 1 {
		for j := 1; j <= s.Replicas; j++ {
			if j != i {
				out = append(out, nsSuffix(j))
			}
		}
	}
	return out
}

func ecsContainer(s Spec, i int, r ecsRefs, nsSuffix func(j int) any) m {
	return m{
		"Name":                   "gwag",
		"Image":                  r.image,
		"Essential":              true,
		"Command":                ecsCommand(s, i, nsSuffix),
		"StopTimeout":            StopGraceSeconds,
		"ReadonlyRootFilesystem": true,
		"PortMappings": l{
			m{"ContainerPort": HTTPPort, "Protocol": "tcp"},
			m{"ContainerPort": CPPort, "Protocol": "tcp"},
			m{"ContainerPort": NATSClient, "Protocol": "tcp"},
			m{"ContainerPort": NATSCluster, "Protocol": "tcp"},
		},
		"MountPoints": l{m{"ContainerPath": "/data", "SourceVolume": "data"}},
		"Secrets":     l{m{"Name": "GWAG_ADMIN_TOKEN", "ValueFrom": r.secretARN}},
		// Liveness only: any HTTP answer. A node without a meta leader
		// answers 503 starting and must not be killed.
		"HealthCheck": m{
			"Command":     l{"CMD", "/gwag", "probe", "--live", fmt.Sprintf("http://localhost:%d/api/health", HTTPPort)},
			"Interval":    10,
			"Timeout":     5,
			"Retries":     6,
			"StartPeriod": 120,
		},
		"LogConfiguration": m{
			"LogDriver": "awslogs",
			"Options": m{
				"awslogs-group":         r.logGroup,
				"awslogs-region":        r.region,
				"awslogs-stream-prefix": "gwag",
			},
		},
	}
}

func ecsTaskDef(s Spec, i int, r ecsRefs, nsSuffix func(j int) any) m {
	return m{
		"Family":                  fmt.Sprintf("gwag-%d", i),
		"RequiresCompatibilities": l{"FARGATE"},
		"NetworkMode":             "awsvpc",
		"Cpu":                     "512",
		"Memory":                  "1024",
		"ExecutionRoleArn":        r.execRole,
		"Volumes": l{m{
			"Name": "data",
			"EFSVolumeConfiguration": m{
				"FilesystemId":      r.fileSystem,
				"TransitEncryption": "ENABLED",
				"AuthorizationConfig": m{
					"AccessPointId": r.accessPoint,
					"IAM":           "DISABLED",
				},
			},
		}},
		"ContainerDefinitions": l{ecsContainer(s, i, r, nsSuffix)},
	}
}

func genECS(s Spec) ([]File, error) {
	ref := func(n string) m { return m{"Ref": n} }
	getAtt := func(n, a string) m { return m{"Fn::GetAtt": l{n, a}} }
	sub := func(x string) m { return m{"Fn::Sub": x} }

	params := m{
		"VpcId":   m{"Type": "AWS::EC2::VPC::Id"},
		"SubnetA": m{"Type": "AWS::EC2::Subnet::Id", "Description": "Subnet for ALB, EFS mount target and tasks. Different AZ from SubnetB."},
		"SubnetB": m{"Type": "AWS::EC2::Subnet::Id"},
		"AssignPublicIp": m{"Type": "String", "Default": "ENABLED", "AllowedValues": l{"ENABLED", "DISABLED"},
			"Description": "ENABLED when the subnets are public. DISABLED needs a NAT gateway or VPC endpoints to pull the image."},
		"AlbCidr":       m{"Type": "String", "Default": "0.0.0.0/0", "Description": "Who may reach the load balancer on port 80."},
		"InternalCidr":  m{"Type": "String", "Default": "10.0.0.0/8", "Description": "Who may reach the control plane (port 50090): your services' network."},
		"NamespaceName": m{"Type": "String", "Default": "gwag.local", "Description": "Cloud Map private DNS namespace. Peers dial gwag-N.<this>."},
	}
	for i := 1; i <= s.Replicas; i++ {
		params[fmt.Sprintf("Image%d", i)] = m{
			"Type": "String", "Default": s.Image,
			"Description": "Image of node " + fmt.Sprint(i) + ". Fargate pulls from a registry: set a registry reference. Change one node per stack update.",
		}
	}

	res := m{}
	res["Cluster"] = m{"Type": "AWS::ECS::Cluster"}
	res["LogGroup"] = m{"Type": "AWS::Logs::LogGroup", "Properties": m{"RetentionInDays": 30}}
	res["AdminSecret"] = m{"Type": "AWS::SecretsManager::Secret", "Properties": m{
		"Description": "gwag admin bearer token (hex)",
		"GenerateSecretString": m{
			// Only 0-9a-f survive: the token must be hex.
			"PasswordLength": 64, "ExcludeUppercase": true, "ExcludePunctuation": true,
			"ExcludeCharacters": "ghijklmnopqrstuvwxyz",
		},
	}}
	res["ExecutionRole"] = m{"Type": "AWS::IAM::Role", "Properties": m{
		"AssumeRolePolicyDocument": m{"Version": "2012-10-17", "Statement": l{m{
			"Effect": "Allow", "Principal": m{"Service": "ecs-tasks.amazonaws.com"}, "Action": "sts:AssumeRole"}}},
		"ManagedPolicyArns": l{"arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"},
		"Policies": l{m{"PolicyName": "read-admin-secret", "PolicyDocument": m{"Version": "2012-10-17", "Statement": l{m{
			"Effect": "Allow", "Action": "secretsmanager:GetSecretValue", "Resource": ref("AdminSecret")}}}}},
	}}

	res["AlbSecurityGroup"] = m{"Type": "AWS::EC2::SecurityGroup", "Properties": m{
		"GroupDescription": "gwag load balancer", "VpcId": ref("VpcId"),
		"SecurityGroupIngress": l{m{"IpProtocol": "tcp", "FromPort": 80, "ToPort": 80, "CidrIp": ref("AlbCidr")}}}}
	res["TaskSecurityGroup"] = m{"Type": "AWS::EC2::SecurityGroup", "Properties": m{
		"GroupDescription": "gwag nodes", "VpcId": ref("VpcId"),
		"SecurityGroupIngress": l{
			m{"IpProtocol": "tcp", "FromPort": CPPort, "ToPort": CPPort, "CidrIp": ref("InternalCidr")},
		}}}
	res["TaskFromAlb"] = m{"Type": "AWS::EC2::SecurityGroupIngress", "Properties": m{
		"GroupId": ref("TaskSecurityGroup"), "SourceSecurityGroupId": ref("AlbSecurityGroup"),
		"IpProtocol": "tcp", "FromPort": HTTPPort, "ToPort": HTTPPort}}
	res["TaskFromTask"] = m{"Type": "AWS::EC2::SecurityGroupIngress", "Properties": m{
		"Description": "NATS routes between nodes",
		"GroupId":     ref("TaskSecurityGroup"), "SourceSecurityGroupId": ref("TaskSecurityGroup"),
		"IpProtocol": "tcp", "FromPort": NATSCluster, "ToPort": NATSCluster}}
	res["EfsSecurityGroup"] = m{"Type": "AWS::EC2::SecurityGroup", "Properties": m{
		"GroupDescription": "gwag EFS", "VpcId": ref("VpcId"),
		"SecurityGroupIngress": l{m{"IpProtocol": "tcp", "FromPort": 2049, "ToPort": 2049, "SourceSecurityGroupId": ref("TaskSecurityGroup")}}}}

	res["FileSystem"] = m{"Type": "AWS::EFS::FileSystem", "Properties": m{
		"Encrypted": true, "PerformanceMode": "generalPurpose", "ThroughputMode": "elastic"}}
	res["MountTargetA"] = m{"Type": "AWS::EFS::MountTarget", "Properties": m{
		"FileSystemId": ref("FileSystem"), "SubnetId": ref("SubnetA"), "SecurityGroups": l{ref("EfsSecurityGroup")}}}
	res["MountTargetB"] = m{"Type": "AWS::EFS::MountTarget", "Properties": m{
		"FileSystemId": ref("FileSystem"), "SubnetId": ref("SubnetB"), "SecurityGroups": l{ref("EfsSecurityGroup")}}}

	res["Namespace"] = m{"Type": "AWS::ServiceDiscovery::PrivateDnsNamespace", "Properties": m{
		"Name": ref("NamespaceName"), "Vpc": ref("VpcId")}}

	res["LoadBalancer"] = m{"Type": "AWS::ElasticLoadBalancingV2::LoadBalancer", "Properties": m{
		"Type": "application", "Scheme": "internet-facing",
		"Subnets": l{ref("SubnetA"), ref("SubnetB")}, "SecurityGroups": l{ref("AlbSecurityGroup")}}}
	res["TargetGroup"] = m{"Type": "AWS::ElasticLoadBalancingV2::TargetGroup", "Properties": m{
		"VpcId": ref("VpcId"), "TargetType": "ip", "Protocol": "HTTP", "Port": HTTPPort,
		"HealthCheckPath": "/api/health", "Matcher": m{"HttpCode": "200"},
		"HealthCheckIntervalSeconds": 10, "HealthCheckTimeoutSeconds": 5,
		"HealthyThresholdCount": 2, "UnhealthyThresholdCount": 2,
		"TargetGroupAttributes": l{m{"Key": "deregistration_delay.timeout_seconds", "Value": "30"}}}}
	res["Listener"] = m{"Type": "AWS::ElasticLoadBalancingV2::Listener", "Properties": m{
		"LoadBalancerArn": ref("LoadBalancer"), "Port": 80, "Protocol": "HTTP",
		"DefaultActions": l{m{"Type": "forward", "TargetGroupArn": ref("TargetGroup")}}}}

	files := []File{}
	for i := 1; i <= s.Replicas; i++ {
		n := fmt.Sprint(i)
		res["AccessPoint"+n] = m{"Type": "AWS::EFS::AccessPoint", "Properties": m{
			"FileSystemId": ref("FileSystem"),
			"PosixUser":    m{"Uid": "65532", "Gid": "65532"},
			"RootDirectory": m{"Path": "/gwag-" + n,
				"CreationInfo": m{"OwnerUid": "65532", "OwnerGid": "65532", "Permissions": "750"}}}}
		res["Discovery"+n] = m{"Type": "AWS::ServiceDiscovery::Service", "Properties": m{
			"Name":        "gwag-" + n,
			"NamespaceId": getAtt("Namespace", "Id"),
			"DnsConfig":   m{"RoutingPolicy": "MULTIVALUE", "DnsRecords": l{m{"Type": "A", "TTL": 10}}}}}

		inTemplate := ecsRefs{
			secretARN: ref("AdminSecret"), fileSystem: ref("FileSystem"), accessPoint: ref("AccessPoint" + n),
			execRole: getAtt("ExecutionRole", "Arn"), logGroup: ref("LogGroup"), image: ref("Image" + n),
			region: ref("AWS::Region"),
		}
		res["TaskDef"+n] = m{"Type": "AWS::ECS::TaskDefinition", "Properties": ecsTaskDef(s, i, inTemplate,
			func(j int) any {
				return sub(fmt.Sprintf("--nats-peer=gwag-%d.${NamespaceName}:%d", j, NATSCluster))
			})}
		res["Service"+n] = m{"Type": "AWS::ECS::Service",
			"DependsOn": l{"Listener", "MountTargetA", "MountTargetB"},
			"Properties": m{
				"Cluster": ref("Cluster"), "LaunchType": "FARGATE", "PlatformVersion": "LATEST",
				"DesiredCount": 1, "TaskDefinition": ref("TaskDef" + n),
				// One task per service: stop the old before starting the
				// new, so two never share a data directory or node name.
				"DeploymentConfiguration": m{"MinimumHealthyPercent": 0, "MaximumPercent": 100,
					"DeploymentCircuitBreaker": m{"Enable": true, "Rollback": true}},
				"HealthCheckGracePeriodSeconds": 180,
				"NetworkConfiguration": m{"AwsvpcConfiguration": m{
					"AssignPublicIp": ref("AssignPublicIp"),
					"Subnets":        l{ref("SubnetA"), ref("SubnetB")},
					"SecurityGroups": l{ref("TaskSecurityGroup")}}},
				"LoadBalancers":     l{m{"ContainerName": "gwag", "ContainerPort": HTTPPort, "TargetGroupArn": ref("TargetGroup")}},
				"ServiceRegistries": l{m{"RegistryArn": getAtt("Discovery"+n, "Arn")}},
			}}

		// Standalone task definition for `aws ecs register-task-definition`.
		td := ecsTaskDef(s, i, ecsRefs{
			secretARN: "REPLACE_ADMIN_SECRET_ARN", fileSystem: "REPLACE_EFS_FILE_SYSTEM_ID",
			accessPoint: "REPLACE_EFS_ACCESS_POINT_ID_NODE_" + n, execRole: "REPLACE_EXECUTION_ROLE_ARN",
			logGroup: "REPLACE_LOG_GROUP_NAME", image: s.Image, region: "REPLACE_REGION",
		}, func(j int) any { return fmt.Sprintf("--nats-peer=gwag-%d.gwag.local:%d", j, NATSCluster) })
		td["Family"] = fmt.Sprintf("gwag-%d", i)
		b, err := marshalLower(td)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: fmt.Sprintf("taskdef-gwag-%d.json", i), Mode: 0o644, Data: b})
	}

	tmpl := m{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Description":              fmt.Sprintf("gwag cluster, %d node(s) on ECS Fargate. Generated by gwag deploy export.", s.Replicas),
		"Parameters":               params,
		"Resources":                res,
		"Outputs": m{
			"LoadBalancerDns": m{"Value": getAtt("LoadBalancer", "DNSName"), "Description": "HTTP endpoint: http://<this>/api/graphql"},
			"AdminSecretArn":  m{"Value": ref("AdminSecret"), "Description": "aws secretsmanager get-secret-value --secret-id <this> --query SecretString --output text"},
			"ClusterName":     m{"Value": ref("Cluster")},
		},
	}
	b, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: "cloudformation.json", Mode: 0o644, Data: append(b, '\n')})
	return files, nil
}

// marshalLower renders the template-form (CloudFormation property
// names, PascalCase) task definition as the camelCase JSON that
// `aws ecs register-task-definition --cli-input-json` takes.
func marshalLower(v any) ([]byte, error) {
	b, err := json.MarshalIndent(lowerKeys(v), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// lowerKeys lower-cases the first letter of every map key, with the
// ECS API's irregular names mapped explicitly.
func lowerKeys(v any) any {
	irregular := map[string]string{
		"EFSVolumeConfiguration": "efsVolumeConfiguration",
		"FilesystemId":           "fileSystemId",
		"IAM":                    "iam",
	}
	switch x := v.(type) {
	case m:
		out := m{}
		for k, val := range x {
			nk := irregular[k]
			if nk == "" {
				nk = string(lowerFirst(k[0])) + k[1:]
			}
			// Values under Options keep their (hyphenated) keys.
			if k == "Options" {
				out[nk] = val
				continue
			}
			out[nk] = lowerKeys(val)
		}
		return out
	case l:
		out := make(l, len(x))
		for i := range x {
			out[i] = lowerKeys(x[i])
		}
		return out
	}
	return v
}

func lowerFirst(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}
