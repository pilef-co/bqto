#!/usr/bin/env bash
set -euo pipefail

export CLOUDSDK_CORE_INTERACTIVE_UX_STYLE=OFF

REGION=us-east1
NETWORK=default
SUBNET=""
PROJECT=""
ASSUME_YES=0
PASSWORD=${BQTO_TEST_PASSWORD:-$(openssl rand -hex 16)}
FAILED=0

usage() {
	cat <<'EOF'
Create or delete the instances bqto is tested against.

Usage:
  scripts/testenv.sh create TARGET [flags]
  scripts/testenv.sh prepare TARGET [flags]
  scripts/testenv.sh connect TARGET [flags]
  scripts/testenv.sh delete TARGET [flags]
  scripts/testenv.sh leftovers [flags]
  scripts/testenv.sh delete-all [flags]

create makes the instance. prepare makes only what targets share (the
reserved range and peering, the Private Service Connect policies), so that
several creates can then run at once. connect runs "go run ./cmd/bqto connect"
against it and keeps the session open until you press Ctrl-C. delete removes it.

Flags:
  --project ID   project to work in (default: the gcloud one)
  --region R     default us-east1
  --network N    default "default"
  --subnet S     subnet for alloydb-psc (default: the network name)
  --yes          do not ask before creating

Targets the free bqto supports:
  cloudsql          Cloud SQL for PostgreSQL, db-f1-micro, private IP     about 0.02 $/hour
  redis-basic       Memorystore for Redis, Basic tier, 1 GB               about 0.05 $/hour
  redis-std         Memorystore for Redis, Standard tier, 1 GB            about 0.07 $/hour
  alloydb-psa       AlloyDB over private services access                  about 0.16 $/hour
  alloydb-psc       AlloyDB over Private Service Connect                  about 0.17 $/hour
  valkey            Memorystore for Valkey, 1 shard, shared-core-nano     about 0.07 $/hour

Targets that need bqto Pro:
  cloudrun          Cloud Run service, internal ingress                   free while idle
  valkey-cluster    Memorystore for Valkey, cluster mode, 1 shard         about 0.07 $/hour
  redis-cluster     Memorystore for Redis Cluster, 1 shard                about 0.07 $/hour
  kafka             Managed Kafka, private cluster, 3 vCPU                about 0.60 $/hour

Targets that need their primary first:
  cloudsql-replica  read replica of cloudsql                              about as much as cloudsql
  alloydb-pool      1-node read pool in alloydb-psa                       about as much as its primary

Everything it creates is named bqto-test-*. Delete leaves the reserved IP range
and the peering in place: they are free and the next run reuses them.
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

run() {
	echo "+ $*" >&2
	"$@"
}

del() {
	echo "+ $*" >&2
	if out=$("$@" 2>&1); then
		[ -n "$out" ] && echo "$out" >&2
		return 0
	fi
	if echo "$out" | grep -qE 'NOT_FOUND|was not found|could not be found|does not exist'; then
		echo "note: it was already gone" >&2
		return 0
	fi
	echo "$out" >&2
	echo "error: delete failed, the resource may still be costing money" >&2
	FAILED=1
}

del_sql() {
	if ! names=$(gcloud sql instances list --project "$PROJECT" --filter="name=$1" --format='value(name)' 2>/dev/null); then
		echo "error: cannot list Cloud SQL instances, $1 may still be costing money" >&2
		FAILED=1
		return 0
	fi
	if [ -z "$names" ]; then
		echo "note: $1 was already gone" >&2
		return 0
	fi
	del gcloud sql instances delete "$1" --project "$PROJECT" --quiet
}

confirm() {
	[ "$ASSUME_YES" = 1 ] && return 0
	printf '%s\nContinue? [y/N] ' "$1" >&2
	read -r answer
	case "$answer" in
	y | Y | yes) return 0 ;;
	esac
	die "cancelled"
}

hint() {
	echo "test with: scripts/testenv.sh connect $TARGET_NAME --project $PROJECT" >&2
}

connect_args() {
	case "$1" in
	cloudsql) echo "--service cloudsql --project $PROJECT --instance bqto-test-sql" ;;
	cloudsql-replica) echo "--service cloudsql --project $PROJECT --instance bqto-test-sql-replica" ;;
	alloydb-pool) echo "--service alloydb --project $PROJECT --instance $REGION/bqto-test-psa/bqto-test-psa-pool" ;;
	cloudrun) echo "--service cloudrun --project $PROJECT --instance $REGION/bqto-test-run" ;;
	redis-basic) echo "--service redis --project $PROJECT --instance $REGION/bqto-test-redis-basic" ;;
	redis-std) echo "--service redis --project $PROJECT --instance $REGION/bqto-test-redis-std" ;;
	alloydb-psa) echo "--service alloydb --project $PROJECT --instance $REGION/bqto-test-psa/bqto-test-psa-primary" ;;
	alloydb-psc) echo "--service alloydb --project $PROJECT --instance $REGION/bqto-test-psc/bqto-test-psc-primary" ;;
	valkey) echo "--service valkey --project $PROJECT --instance $REGION/bqto-test-valkey" ;;
	valkey-cluster) echo "--service valkey --project $PROJECT --instance $REGION/bqto-test-valkey-cluster" ;;
	redis-cluster) echo "--service redis --project $PROJECT --instance $REGION/bqto-test-redis-cluster" ;;
	kafka) echo "--service kafka --project $PROJECT --instance $REGION/bqto-test-kafka" ;;
	*) die "unknown target $1" ;;
	esac
}

psa_range() {
	if ! gcloud compute addresses describe bqto-test-psa --global --project "$PROJECT" >/dev/null 2>&1; then
		run gcloud compute addresses create bqto-test-psa --global --purpose=VPC_PEERING \
			--prefix-length=16 --network="$NETWORK" --project "$PROJECT"
	fi
	ranges=$(gcloud services vpc-peerings list --network="$NETWORK" --project "$PROJECT" \
		--format='value(reservedPeeringRanges)' 2>/dev/null | tr ';' ',' || true)
	if [ -z "$ranges" ]; then
		run gcloud services vpc-peerings connect --service=servicenetworking.googleapis.com \
			--ranges=bqto-test-psa --network="$NETWORK" --project "$PROJECT"
		return
	fi
	case ",$ranges," in
	*,bqto-test-psa,*) return ;;
	esac
	run gcloud services vpc-peerings update --service=servicenetworking.googleapis.com \
		--ranges="$ranges,bqto-test-psa" --network="$NETWORK" --project "$PROJECT" --force
}

policy() {
	name=$1
	class=$2
	if ! gcloud network-connectivity service-connection-policies describe "$name" \
		--region="$REGION" --project "$PROJECT" >/dev/null 2>&1; then
		run gcloud network-connectivity service-connection-policies create "$name" \
			--network="projects/$PROJECT/global/networks/$NETWORK" \
			--service-class="$class" --region="$REGION" \
			--subnets="projects/$PROJECT/regions/$REGION/subnetworks/$SUBNET" \
			--psc-connection-limit=5 --project "$PROJECT"
	fi
}

prepare() {
	case "$1" in
	cloudsql)
		run gcloud services enable sqladmin.googleapis.com servicenetworking.googleapis.com --project "$PROJECT"
		psa_range
		;;
	alloydb-psa)
		run gcloud services enable alloydb.googleapis.com servicenetworking.googleapis.com --project "$PROJECT"
		psa_range
		;;
	alloydb-psc)
		run gcloud services enable alloydb.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
		policy bqto-test-alloydb google-cloud-alloydb
		;;
	valkey | valkey-cluster)
		run gcloud services enable memorystore.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
		policy bqto-test-memorystore gcp-memorystore
		;;
	redis-cluster)
		run gcloud services enable redis.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
		policy bqto-test-redis-cluster gcp-memorystore-redis
		;;
	cloudrun) run gcloud services enable run.googleapis.com --project "$PROJECT" ;;
	redis-basic | redis-std) run gcloud services enable redis.googleapis.com --project "$PROJECT" ;;
	kafka) run gcloud services enable managedkafka.googleapis.com --project "$PROJECT" ;;
	cloudsql-replica | alloydb-pool) ;;
	*) die "unknown target $1" ;;
	esac
}

create_cloudsql() {
	confirm "Creates Cloud SQL bqto-test-sql (db-f1-micro, PostgreSQL, private IP) in $PROJECT/$REGION, about 0.02 \$/hour."
	run gcloud services enable sqladmin.googleapis.com servicenetworking.googleapis.com --project "$PROJECT"
	psa_range
	run gcloud sql instances create bqto-test-sql --database-version=POSTGRES_17 --edition=ENTERPRISE --tier=db-f1-micro \
		--region="$REGION" --no-assign-ip --network="projects/$PROJECT/global/networks/$NETWORK" \
		--root-password="$PASSWORD" --project "$PROJECT"
	run gcloud sql instances describe bqto-test-sql --project "$PROJECT" \
		--format='value(state,ipAddresses[0].ipAddress,settings.ipConfiguration.privateNetwork)'
	echo "password: $PASSWORD" >&2
	hint
}

create_cloudsql_replica() {
	confirm "Creates the read replica bqto-test-sql-replica of bqto-test-sql (db-f1-micro) in $PROJECT/$REGION, about as much as its primary."
	run gcloud sql instances create bqto-test-sql-replica --master-instance-name=bqto-test-sql \
		--edition=ENTERPRISE --tier=db-f1-micro --region="$REGION" --no-assign-ip \
		--network="projects/$PROJECT/global/networks/$NETWORK" --project "$PROJECT"
	run gcloud sql instances describe bqto-test-sql-replica --project "$PROJECT" \
		--format='value(state,instanceType,masterInstanceName,ipAddresses[0].ipAddress)'
	hint
}

create_alloydb_pool() {
	confirm "Creates the 1-node read pool bqto-test-psa-pool (c4a-highmem-1) in the AlloyDB cluster bqto-test-psa, about as much as its primary."
	run gcloud alloydb instances create bqto-test-psa-pool --cluster=bqto-test-psa \
		--region="$REGION" --instance-type=READ_POOL --read-pool-node-count=1 \
		--machine-type=c4a-highmem-1 --project "$PROJECT"
	run gcloud alloydb instances describe bqto-test-psa-pool --cluster=bqto-test-psa \
		--region="$REGION" --project "$PROJECT" --format='value(state,instanceType,ipAddress)'
	hint
}

create_cloudrun() {
	confirm "Deploys the Cloud Run service bqto-test-run with internal ingress in $PROJECT/$REGION. It is free while idle."
	run gcloud services enable run.googleapis.com --project "$PROJECT"
	run gcloud run deploy bqto-test-run --image=us-docker.pkg.dev/cloudrun/container/hello \
		--region="$REGION" --ingress=internal --no-allow-unauthenticated --project "$PROJECT"
	run gcloud run services describe bqto-test-run --region="$REGION" --project "$PROJECT" \
		--format='value(status.url,spec.template.metadata.annotations["run.googleapis.com/ingress"])'
	hint
}

create_redis() {
	tier=$1
	name=$2
	price=$3
	confirm "Creates Memorystore for Redis $name (1 GB, $tier) in $PROJECT/$REGION, about $price \$/hour."
	run gcloud services enable redis.googleapis.com --project "$PROJECT"
	run gcloud redis instances create "$name" --size=1 --tier="$tier" \
		--region="$REGION" --network="$NETWORK" --project "$PROJECT"
	run gcloud redis instances describe "$name" --region="$REGION" --project "$PROJECT" \
		--format='value(host,port,state)'
	hint
}

alloydb_primary() {
	cluster=$1
	shift
	for machine in c4a-highmem-1 n2-highmem-2; do
		echo "+ gcloud alloydb instances create $cluster-primary --cluster=$cluster --region=$REGION --machine-type=$machine $* --project $PROJECT" >&2
		if out=$(gcloud alloydb instances create "$cluster-primary" --cluster="$cluster" \
			--region="$REGION" --instance-type=PRIMARY --machine-type="$machine" "$@" --project "$PROJECT" 2>&1); then
			echo "$out" >&2
			return 0
		fi
		echo "$out" >&2
		case "$out" in
		*"does not have enough resources"* | *RESOURCE_POOL_EXHAUSTED* | *stockout*)
			echo "note: no $machine left in $REGION, trying the next machine type" >&2
			gcloud alloydb instances delete "$cluster-primary" --cluster="$cluster" --region="$REGION" \
				--project "$PROJECT" --quiet >/dev/null 2>&1 || true
			;;
		*) return 1 ;;
		esac
	done
	return 1
}

create_alloydb() {
	mode=$1
	price=$2
	confirm "Creates an AlloyDB cluster and a c4a-highmem-1 instance ($mode) in $PROJECT/$REGION, about $price \$/hour (n2-highmem-2, about twice that, if c4a is out of stock)."
	run gcloud services enable alloydb.googleapis.com servicenetworking.googleapis.com --project "$PROJECT"
	if [ "$mode" = psc ]; then
		run gcloud services enable networkconnectivity.googleapis.com --project "$PROJECT"
		policy bqto-test-alloydb google-cloud-alloydb
		run gcloud alloydb clusters create bqto-test-psc --region="$REGION" \
			--password="$PASSWORD" --enable-private-service-connect --project "$PROJECT"
		alloydb_primary bqto-test-psc --allowed-psc-projects="$PROJECT" \
			--psc-auto-connections="network=projects/$PROJECT/global/networks/$NETWORK,project=$PROJECT"
		run gcloud alloydb instances describe bqto-test-psc-primary --cluster=bqto-test-psc \
			--region="$REGION" --project "$PROJECT" \
			--format='yaml(pscInstanceConfig.pscAutoConnections)'
		echo "password: $PASSWORD" >&2
		hint
		return
	fi
	psa_range
	run gcloud alloydb clusters create bqto-test-psa --region="$REGION" --network="$NETWORK" \
		--password="$PASSWORD" --project "$PROJECT"
	alloydb_primary bqto-test-psa
	echo "password: $PASSWORD" >&2
	hint
}

create_valkey() {
	confirm "Creates Memorystore for Valkey bqto-test-valkey (cluster mode disabled, 1 shard, shared-core-nano) in $PROJECT/$REGION, about 0.07 \$/hour."
	run gcloud services enable memorystore.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
	policy bqto-test-memorystore gcp-memorystore
	run gcloud memorystore instances create bqto-test-valkey --location="$REGION" \
		--mode=CLUSTER_DISABLED --shard-count=1 --node-type=shared-core-nano \
		--psc-auto-connections="network=projects/$PROJECT/global/networks/$NETWORK,projectId=$PROJECT" \
		--project "$PROJECT"
	run gcloud memorystore instances describe bqto-test-valkey --location="$REGION" --project "$PROJECT" \
		--format='yaml(state,mode,endpoints)'
	hint
}

create_valkey_cluster() {
	confirm "Creates Memorystore for Valkey bqto-test-valkey-cluster (CLUSTER mode, 1 shard, no replicas, shared-core-nano) in $PROJECT/$REGION, about 0.07 \$/hour."
	run gcloud services enable memorystore.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
	policy bqto-test-memorystore gcp-memorystore
	run gcloud memorystore instances create bqto-test-valkey-cluster --location="$REGION" \
		--mode=CLUSTER --shard-count=1 --replica-count=0 --node-type=shared-core-nano \
		--psc-auto-connections="network=projects/$PROJECT/global/networks/$NETWORK,projectId=$PROJECT" \
		--project "$PROJECT"
	run gcloud memorystore instances describe bqto-test-valkey-cluster --location="$REGION" --project "$PROJECT" \
		--format='yaml(state,mode,shardCount,discoveryEndpoints,endpoints)'
	hint
}

create_redis_cluster() {
	confirm "Creates Memorystore for Redis Cluster bqto-test-redis-cluster (1 shard, no replicas, redis-shared-core-nano) in $PROJECT/$REGION, about 0.07 \$/hour."
	run gcloud services enable redis.googleapis.com networkconnectivity.googleapis.com --project "$PROJECT"
	policy bqto-test-redis-cluster gcp-memorystore-redis
	run gcloud redis clusters create bqto-test-redis-cluster --region="$REGION" \
		--network="projects/$PROJECT/global/networks/$NETWORK" \
		--shard-count=1 --replica-count=0 --node-type=redis-shared-core-nano \
		--project "$PROJECT"
	run gcloud redis clusters describe bqto-test-redis-cluster --region="$REGION" --project "$PROJECT" \
		--format='yaml(state,shardCount,discoveryEndpoints)'
	hint
}

create_kafka() {
	price=$1
	confirm "Creates the private Managed Kafka cluster bqto-test-kafka (3 vCPU, 3 GiB) and one topic in $PROJECT/$REGION, about $price \$/hour."
	run gcloud services enable managedkafka.googleapis.com --project "$PROJECT"
	run gcloud managed-kafka clusters create bqto-test-kafka --location="$REGION" \
		--cpu=3 --memory=3GiB \
		--subnets="projects/$PROJECT/regions/$REGION/subnetworks/$SUBNET" \
		--project "$PROJECT"
	run gcloud managed-kafka topics create bqto-test-topic --cluster=bqto-test-kafka \
		--location="$REGION" --partitions=1 --replication-factor=1 --project "$PROJECT"
	run gcloud managed-kafka clusters describe bqto-test-kafka --location="$REGION" --project "$PROJECT" \
		--format='value(state,bootstrapAddress)'
	hint
}

delete_all() {
	confirm "Deletes every bqto-test-* resource in $PROJECT, in every region."

	for uri in $(gcloud redis instances list --region=- --project "$PROJECT" --uri 2>/dev/null | grep 'bqto-test-'); do
		del gcloud redis instances delete "$uri" --quiet
	done
	for uri in $(gcloud redis clusters list --region=- --project "$PROJECT" --uri 2>/dev/null | grep 'bqto-test-'); do
		del gcloud redis clusters delete "$uri" --quiet
	done
	for full in $(gcloud memorystore instances list --location=- --project "$PROJECT" --format='value(name)' 2>/dev/null | grep 'bqto-test-'); do
		del gcloud memorystore instances delete "${full##*/}" --location="$(region_of "$full")" --project "$PROJECT" --quiet
	done
	for name in $(gcloud sql instances list --project "$PROJECT" --format='value(name)' 2>/dev/null | grep '^bqto-test-'); do
		del gcloud sql instances delete "$name" --project "$PROJECT" --quiet
	done
	for full in $(gcloud managed-kafka clusters list --location=- --project "$PROJECT" --format='value(name)' 2>/dev/null | grep 'bqto-test-'); do
		del gcloud managed-kafka clusters delete "${full##*/}" --location="$(region_of "$full")" --project "$PROJECT" --quiet
	done
	for uri in $(gcloud alloydb clusters list --region=- --project "$PROJECT" --uri 2>/dev/null | grep 'bqto-test-'); do
		alloydb_del "${uri##*/}" "$(region_of "$uri")" gcloud alloydb clusters delete "$uri" --force --quiet
	done
	gcloud run services list --project "$PROJECT" --format='value(metadata.name,metadata.labels."cloud.googleapis.com/location")' 2>/dev/null |
		grep '^bqto-test-' | while read -r name loc; do
		del gcloud run services delete "$name" --region="$loc" --project "$PROJECT" --quiet
	done
	leftovers
}

alloydb_idle() {
	for _ in {1..90}; do
		busy=$(gcloud alloydb operations list --region="$2" --project "$PROJECT" \
			--filter="NOT done:true AND metadata.target:/clusters/$1" --format='value(name)' 2>/dev/null </dev/null) || busy=""
		[ -z "$busy" ] && return 0
		echo "waiting for a running operation on AlloyDB cluster $1..." >&2
		sleep 10
	done
	echo "warn: AlloyDB cluster $1 still has a running operation" >&2
}

alloydb_del() {
	cluster=$1 region=$2
	shift 2
	prev=$FAILED
	for _ in 1 2 3; do
		alloydb_idle "$cluster" "$region"
		FAILED=0
		del "$@"
		[ "$FAILED" = 0 ] && break
	done
	[ "$prev" = 1 ] && FAILED=1
	return 0
}

region_of() {
	printf '%s' "$1" | sed -n 's#.*/locations/\([^/]*\)/.*#\1#p; s#.*/regions/\([^/]*\)/.*#\1#p'
}

leftovers() {
	list "Cloud SQL" gcloud sql instances list --project "$PROJECT" --format='value(name.basename(),state)'
	list "Cloud Run" gcloud run services list --project "$PROJECT" --format='value(metadata.name)'
	list "Redis" gcloud redis instances list --region=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "Redis Cluster" gcloud redis clusters list --region=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "Valkey" gcloud memorystore instances list --location=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "Kafka" gcloud managed-kafka clusters list --location=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "AlloyDB" gcloud alloydb clusters list --region=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "AlloyDB instances" gcloud alloydb instances list --cluster=- --region=- --project "$PROJECT" --format='value(name.basename(),state)'
	list "bastions" gcloud compute instances list --project "$PROJECT" --format='value(name,status)'
	list "bastion firewall rules" gcloud compute firewall-rules list --project "$PROJECT" \
		--filter='name~^bqto-' --format='value(name,direction,destinationRanges.list())'
}

list() {
	label=$1
	shift
	echo "-- $label" >&2
	if ! out=$("$@" 2>/dev/null </dev/null); then
		echo "(cannot list: the API is off or the account cannot see it)" >&2
		return 0
	fi
	echo "$out" | grep '^bqto-' >&2 || true
}

action=${1:-}
case "$action" in
"" | -h | --help | help)
	usage
	[ -n "$action" ] && exit 0
	exit 1
	;;
create | prepare | connect | delete | delete-all | leftovers) ;;
*) die "unknown action $action, see --help" ;;
esac
shift

target=""
if [ "$action" != leftovers ] && [ "$action" != delete-all ]; then
	target=${1:-}
	[ -n "$target" ] || die "$action needs a target, see --help"
	shift
fi

while [ $# -gt 0 ]; do
	value=${2:-}
	case "$1" in
	--project | --region | --network | --subnet)
		[ -n "$value" ] || die "$1 needs a value"
		case "$1" in
		--project) PROJECT=$value ;;
		--region) REGION=$value ;;
		--network) NETWORK=$value ;;
		--subnet) SUBNET=$value ;;
		esac
		shift 2
		;;
	--yes)
		ASSUME_YES=1
		shift
		;;
	*) die "unknown flag $1" ;;
	esac
done

[ -n "$PROJECT" ] || PROJECT=$(gcloud config get-value project 2>/dev/null || true)
[ -n "$PROJECT" ] && [ "$PROJECT" != "(unset)" ] || die "no project: pass --project ID"
[ -n "$SUBNET" ] || SUBNET=$NETWORK
TARGET_NAME=$target

case "$action" in
prepare) prepare "$target" ;;
create)
	case "$target" in
	cloudsql) create_cloudsql ;;
	cloudsql-replica) create_cloudsql_replica ;;
	alloydb-pool) create_alloydb_pool ;;
	cloudrun) create_cloudrun ;;
	redis-basic) create_redis basic bqto-test-redis-basic 0.05 ;;
	redis-std) create_redis standard bqto-test-redis-std 0.07 ;;
	alloydb-psa) create_alloydb psa 0.16 ;;
	alloydb-psc) create_alloydb psc 0.17 ;;
	valkey) create_valkey ;;
	valkey-cluster) create_valkey_cluster ;;
	redis-cluster) create_redis_cluster ;;
	kafka) create_kafka 0.60 ;;
	*) die "unknown target $target" ;;
	esac
	;;
delete)
	case "$target" in
	cloudsql) del_sql bqto-test-sql ;;
	cloudsql-replica) del_sql bqto-test-sql-replica ;;
	alloydb-pool)
		alloydb_del bqto-test-psa "$REGION" gcloud alloydb instances delete bqto-test-psa-pool --cluster=bqto-test-psa --region="$REGION" --project "$PROJECT" --quiet
		;;
	cloudrun) del gcloud run services delete bqto-test-run --region="$REGION" --project "$PROJECT" --quiet ;;
	redis-basic) del gcloud redis instances delete bqto-test-redis-basic --region="$REGION" --project "$PROJECT" --quiet ;;
	redis-std) del gcloud redis instances delete bqto-test-redis-std --region="$REGION" --project "$PROJECT" --quiet ;;
	alloydb-psa)
		alloydb_del bqto-test-psa "$REGION" gcloud alloydb clusters delete bqto-test-psa --region="$REGION" --force --project "$PROJECT" --quiet
		;;
	alloydb-psc)
		alloydb_del bqto-test-psc "$REGION" gcloud alloydb clusters delete bqto-test-psc --region="$REGION" --force --project "$PROJECT" --quiet
		del gcloud network-connectivity service-connection-policies delete bqto-test-alloydb --region="$REGION" --project "$PROJECT" --quiet
		;;
	redis-cluster)
		del gcloud redis clusters delete bqto-test-redis-cluster --region="$REGION" --project "$PROJECT" --quiet
		del gcloud network-connectivity service-connection-policies delete bqto-test-redis-cluster --region="$REGION" --project "$PROJECT" --quiet
		;;
	valkey-cluster)
		del gcloud memorystore instances delete bqto-test-valkey-cluster --location="$REGION" --project "$PROJECT" --quiet
		del gcloud network-connectivity service-connection-policies delete bqto-test-memorystore --region="$REGION" --project "$PROJECT" --quiet
		;;
	valkey)
		del gcloud memorystore instances delete bqto-test-valkey --location="$REGION" --project "$PROJECT" --quiet
		del gcloud network-connectivity service-connection-policies delete bqto-test-memorystore --region="$REGION" --project "$PROJECT" --quiet
		;;
	kafka) del gcloud managed-kafka clusters delete bqto-test-kafka --location="$REGION" --project "$PROJECT" --quiet ;;
	*) die "unknown target $target" ;;
	esac
	leftovers
	;;
connect)
	args=$(connect_args "$target")
	echo "+ go run ./cmd/bqto connect $args --duration 10m" >&2
	# shellcheck disable=SC2086
	exec go run ./cmd/bqto connect $args --duration 10m
	;;
leftovers) leftovers ;;
delete-all) delete_all ;;
esac

exit "$FAILED"
