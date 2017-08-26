package anser

import (
	"fmt"
	"sync"

	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/job"
	"github.com/mongodb/amboy/registry"
	"github.com/mongodb/grip"
	"gopkg.in/mgo.v2/bson"
)

func init() {
	registry.AddJobType("aggregate-migration-generator",
		func() amboy.Job { return makeAggregateGenerator() })
}

func NewAggregateMigrationGenerator(e Environment, opts GeneratorOptions, opName string) MigrationGenerator {
	j := makeAggregateGenerator()
	j.SetID(opts.JobID)
	j.SetDependency(opts.dependency())
	j.MigrationHelper = NewMigrationHelper(e)
	j.NS = opts.NS
	j.Query = opts.Query
	j.ProcessorName = opName

	return j
}

func makeAggregateGenerator() *aggregateMigrationGenerator {
	return &aggregateMigrationGenerator{
		MigrationHelper: &migrationBase{},
		Base: job.Base{
			JobType: amboy.JobType{
				Name:    "aggregate-migration-generator",
				Version: 0,
				Format:  amboy.BSON,
			},
		},
	}
}

type aggregateMigrationGenerator struct {
	NS              Namespace                `bson:"ns" json:"ns" yaml:"ns"`
	Query           map[string]interface{}   `bson:"source_query" json:"source_query" yaml:"source_query"`
	ProcessorName   string                   `bson:"processor_name" json:"processor_name" yaml:"processor_name"`
	Migrations      []*aggregateMigrationJob `bson:"migrations" json:"migrations" yaml:"migrations"`
	job.Base        `bson:"job_base" json:"job_base" yaml:"job_base"`
	MigrationHelper `bson:"-" json:"-" yaml:"-"`
	mu              sync.Mutex
}

func (j *aggregateMigrationGenerator) Run() {
	defer j.MarkComplete()

	env := j.Env()

	network, err := env.GetDependencyNetwork()
	if err != nil {
		j.AddError(err)
		return
	}

	session, err := env.GetSession()
	if err != nil {
		j.AddError(err)
		return
	}
	defer session.Close()

	coll := session.DB(j.NS.DB).C(j.NS.Collection)
	iter := coll.Find(j.Query).Select(bson.M{"_id": 1}).Iter()

	doc := struct {
		ID interface{} `bson:"_id"`
	}{}

	ids := []string{}
	j.mu.Lock()
	defer j.mu.Unlock()
	for iter.Next(&doc) {
		m := NewAggregateMigration(env, AggregateProducer{
			// ID:            doc.ID,
			ProcessorName: j.ProcessorName,
			Migration:     j.ID(),
			Namespace:     j.NS,
		}).(*aggregateMigrationJob)

		dep, err := NewMigrationDependencyManager(env, j.ID(), j.Query, j.NS)
		if err != nil {
			j.AddError(err)
			continue
		}
		m.SetDependency(dep)
		m.SetID(fmt.Sprintf("%s.%v.%d", j.ID(), doc.ID, len(ids)))
		ids = append(ids, m.ID())
		j.Migrations = append(j.Migrations, m)
	}

	network.AddGroup(j.ID(), ids)

	if err := iter.Close(); err != nil {
		j.AddError(err)
		return
	}
}

func (j *aggregateMigrationGenerator) Jobs() <-chan amboy.Job {
	env := j.Env()

	j.mu.Lock()
	defer j.mu.Unlock()

	jobs := make(chan amboy.Job, len(j.Migrations))
	for _, job := range j.Migrations {
		jobs <- job
	}
	close(jobs)

	out, err := generator(env, j.ID(), jobs)
	grip.CatchError(err)
	grip.Infof("produced %d tasks for migration %s", len(j.Migrations), j.ID())
	j.Migrations = []*aggregateMigrationJob{}
	return out
}
