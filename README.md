# Qdrant Backup

This script can automatically back up different collections on a Qdrant cluster and upload snapshots to a S3 bucket.

## Features

+ Automatic discovery of Qdrant nodes using Kubernetes headless services.
+ Snapshots all shards of a collection.
+ Compresses snapshots using gzip and uploads them to a S3 bucket.

## How to use?

1. You need a Qdrant cluster deployed on a Kubernetes cluster.
2. You need a headless service containing all qdrant nodes.
3. Build [Dockerfile](Dockerfile) and push it to an image registry.
4. Deploy it as a cronjob (or any other type of workload) to the same Kubernetes cluster. [Sample manifest](backup.yaml) is included.

## Configurations

All configuration parameters should be set using environment variables:

|             Name             |                  Description                  |               Example               |
|:----------------------------:|:---------------------------------------------:|:-----------------------------------:|
|  OBJECT_STORAGE_ACCESS_KEY   |          Access key id of S3 bucket           |                  -                  |
| OBJECT_STORAGE_ACCESS_SECRET |        Access key secret of S3 bucket         |                  -                  |
|  OBJECT_STORAGE_BUCKET_NAME  |                S3 bucket name                 |           qdrant-backups            |
|    OBJECT_STORAGE_ADDRESS    |              S3 bucket endpoint               |     ams3.digitaloceanspaces.com     |
|    OBJECT_STORAGE_REGION     |               S3 bucket region                |                ams3                 |
|         QDRANT_TOKEN         |                Qdrant API key                 |                  -                  |
| QDRANT_GRPC_SERVICE_ADDRESS  |        Qdrant headless service address        |       qdrant-headless.qdrant        |
|       QDRANT_GRPC_PORT       |               Qdrant GRPC Port                |                6334                 |
|       QDRANT_HTTP_PORT       |               Qdrant HTTP Port                |                6333                 |
|    COLLECTIONS_TO_BACKUP     | Comma-separated list of collections to backup | collection1,collection2,collection3 |

## How to restore?
1. Download compressed snapshots on a temporary server (or pod), uncompress them using `gzip -d ${PATH}`.
2. Restore snapshot using the following command:
```shell
curl -X POST "http://${QDRANT_REST_ADDRESS}/collections/${COLLECTION_NAME}/snapshots/upload?priority=snapshot" \
   -H "api-key: ${QDRANT_API_KEY}" \
   -H "Content-Type:multipart/form-data" \
   -F "snapshot=@${PATH}"
```

