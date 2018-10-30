package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go-v2/service/elb"
	"github.com/aws/aws-sdk-go-v2/service/elb/elbiface"
	"github.com/aws/aws-sdk-go-v2/service/elbv2"
	"github.com/aws/aws-sdk-go-v2/service/elbv2/elbv2iface"
)

type expander struct {
	q  chan *leaf
	wg *sync.WaitGroup
}

func (e expander) enqueue(l *leaf) {
	if verbose {
		log.Printf("enqueue %T", l.v)
	}
	e.wg.Add(1)
	e.q <- l
}

func (e expander) expand(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	q := make(chan *leaf)
	go func() {
		ls := []*leaf{}
		for {
			var l *leaf
			if len(ls) > 0 {
				l = ls[0]
			}
			select {
			case l := <-e.q:
				ls = append(ls, l)
			case q <- l:
				if len(ls) > 0 {
					ls = ls[1:]
				}
			case <-done:
				close(q)
				return
			}
		}
	}()

	ws := []string{"mary", "chad", "lucy", "greg", "faye", "bort"}
	for _, w := range ws {
		go func(w string) {
			for l := range q {
				if l == nil {
					continue
				}
				if verbose {
					log.Printf("%s: expand %T", w, l.v)
				}
				if err := l.expand(); err != nil {
					log.Printf("%s: %s", w, err)
				}
				e.wg.Done()
			}
		}(w)
	}

	select {
	case <-done:
	case <-ctx.Done():
	}
}

type leaf struct {
	v  interface{}
	d  int
	cn []*leaf
}

func (l *leaf) spawn(v interface{}) {
	c := &leaf{v: v, d: l.d + 1, cn: []*leaf{}}
	l.cn = append(l.cn, c)
	exr.enqueue(c) // global expander
}

func (l *leaf) expand() error {
	switch v := l.v.(type) {
	case []string:
		if err := l.vpcs(v); err != nil {
			return err
		}
	case ec2.Vpc:
		if err := l.subnets(*v.VpcId); err != nil {
			return err
		}
		if err := l.igws(*v.VpcId); err != nil {
			return err
		}
		l.elbs(*v.VpcId)
	case ec2.Subnet:
		if err := l.nats(*v.SubnetId); err != nil {
			return err
		}
		l.asgs(*v.SubnetId)
	}
	return nil
}

func (l *leaf) writeTo(w io.Writer) {
	in := fmt.Sprintf(fmt.Sprintf("%%%ds", l.d*2), "")
	switch v := l.v.(type) {
	case ec2.Vpc:
		fmt.Fprintf(w, "# %s\f%s\t%s\n", getName(v.Tags), *v.VpcId,
			*v.CidrBlock)
	case ec2.Subnet:
		pub := map[bool]string{true: "public"}
		fmt.Fprintf(w, in+"%s\t%s\t%s\t%s\n", *v.SubnetId, *v.CidrBlock,
			*v.AvailabilityZone, pub[*v.MapPublicIpOnLaunch])
	case autoscaling.Group:
		fmt.Fprintf(w, in+"asg %s\n", *v.AutoScalingGroupName)
	case ec2.InternetGateway:
		fmt.Fprintf(w, in+"%s\n", *v.InternetGatewayId)
	case ec2.NatGateway:
		fmt.Fprintf(w, in+"%s\n", *v.NatGatewayId)
	case elbv2.LoadBalancer:
		t := string(v.Type)[0:1]
		fmt.Fprintf(w, in+"%slb %s\t%s\n", t, *v.LoadBalancerName, v.Scheme)
	case elb.LoadBalancerDescription:
		fmt.Fprintf(w, in+"elb %s\t%s\n", *v.LoadBalancerName, *v.Scheme)
		for _, ld := range v.ListenerDescriptions {
			l := ld.Listener
			fmt.Fprintf(w, in+"  \t%s %d\t->\t%s %d\n", *l.Protocol,
				*l.LoadBalancerPort, *l.InstanceProtocol, *l.InstancePort)
		}
	}

	for _, c := range l.cn {
		c.writeTo(w)
	}

	if _, ok := l.v.(ec2.Vpc); ok {
		fmt.Fprint(w, "\f")
	}
}

func (l *leaf) vpcs(ids []string) error {
	opt := &ec2.DescribeVpcsInput{}
	if len(ids) > 0 {
		opt.VpcIds = ids
	}

	rsp, err := svc.ec2.DescribeVpcsRequest(opt).Send()
	if err != nil {
		return err
	}

	for _, v := range rsp.Vpcs {
		l.spawn(v)
	}
	return nil
}

func (l *leaf) subnets(id string) error {
	f := ec2.Filter{
		Name:   aws.String("vpc-id"),
		Values: []string{id},
	}

	rsp, err := svc.ec2.DescribeSubnetsRequest(&ec2.DescribeSubnetsInput{
		Filters: []ec2.Filter{f},
	}).Send()
	if err != nil {
		return err
	}

	for _, v := range rsp.Subnets {
		l.spawn(v)
	}
	return nil
}

func (l *leaf) asgs(id string) {
	for _, v := range all.asgs {
		if strings.Contains(*v.VPCZoneIdentifier, id) {
			l.spawn(v)
		}
	}
}

func (l *leaf) elbs(id string) {
	for _, v := range all.elbs {
		if *v.VPCId == id {
			l.spawn(v)
		}
	}
	for _, v := range all.elbv2s {
		if *v.VpcId == id {
			l.spawn(v)
		}
	}
}

func (l *leaf) igws(id string) error {
	f := ec2.Filter{
		Name:   aws.String("attachment.vpc-id"),
		Values: []string{id},
	}

	rsp, err := svc.ec2.DescribeInternetGatewaysRequest(&ec2.DescribeInternetGatewaysInput{
		Filters: []ec2.Filter{f},
	}).Send()
	if err != nil {
		return err
	}

	for _, g := range rsp.InternetGateways {
		l.spawn(g)
	}
	return nil
}

func (l *leaf) nats(id string) error {
	f := ec2.Filter{
		Name:   aws.String("subnet-id"),
		Values: []string{id},
	}

	rsp, err := svc.ec2.DescribeNatGatewaysRequest(&ec2.DescribeNatGatewaysInput{
		Filter: []ec2.Filter{f},
	}).Send()
	if err != nil {
		return err
	}

	for _, g := range rsp.NatGateways {
		l.spawn(g)
	}
	return nil
}

func getAsgs() ([]autoscaling.Group, error) {
	req := svc.asg.DescribeAutoScalingGroupsRequest(nil)
	p := req.Paginate()
	asgs := []autoscaling.Group{}
	for p.Next() {
		asgs = append(asgs, p.CurrentPage().AutoScalingGroups...)
	}
	return asgs, p.Err()
}

func getElbs() ([]elb.LoadBalancerDescription, error) {
	req := svc.elb.DescribeLoadBalancersRequest(nil)
	p := req.Paginate()
	elbs := []elb.LoadBalancerDescription{}
	for p.Next() {
		elbs = append(elbs, p.CurrentPage().LoadBalancerDescriptions...)
	}
	return elbs, p.Err()
}

func getElbv2s() ([]elbv2.LoadBalancer, error) {
	req := svc.elbv2.DescribeLoadBalancersRequest(nil)
	p := req.Paginate()
	elbs := []elbv2.LoadBalancer{}
	for p.Next() {
		elbs = append(elbs, p.CurrentPage().LoadBalancers...)
	}
	return elbs, p.Err()
}

func getName(tags []ec2.Tag) string {
	s := "name unknown"
	for _, t := range tags {
		if *t.Key == "Name" {
			s = *t.Value
			break
		}
	}
	return s
}

var (
	all = struct {
		asgs   []autoscaling.Group
		elbs   []elb.LoadBalancerDescription
		elbv2s []elbv2.LoadBalancer
	}{}

	exr expander

	svc = struct {
		asg   autoscalingiface.AutoScalingAPI
		ec2   ec2iface.EC2API
		elb   elbiface.ELBAPI
		elbv2 elbv2iface.ELBV2API
	}{}

	verbose bool
)

func init() {
	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		log.Fatal(err)
	}
	svc.ec2 = ec2.New(cfg)
	svc.asg = autoscaling.New(cfg)
	svc.elb = elb.New(cfg)
	svc.elbv2 = elbv2.New(cfg)

	exr = expander{
		q:  make(chan *leaf, 27),
		wg: &sync.WaitGroup{},
	}
}

func main() {
	log.SetFlags(0)
	flag.BoolVar(&verbose, "v", false, "verbose")
	flag.Parse()

	errs := make(chan error)
	go func() {
		asgs, err := getAsgs()
		all.asgs = asgs
		errs <- err
	}()
	go func() {
		elbs, err := getElbs()
		all.elbs = elbs
		errs <- err
	}()
	go func() {
		elbs, err := getElbv2s()
		all.elbv2s = elbs
		errs <- err
	}()
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			log.Fatal(err)
		}
	}

	l := &leaf{d: -2}
	l.spawn(flag.Args())

	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		cancel()
	}()
	exr.expand(ctx)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	l.writeTo(w)
	w.Flush()
}
