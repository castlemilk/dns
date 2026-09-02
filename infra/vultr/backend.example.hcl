# Supply these values from the approved remote state store; do not commit a
# populated copy. R2, Vultr Object Storage, and other S3-compatible services
# also require their endpoint and skip-validation settings.
bucket = "REPLACE_ME"
key    = "simpledns/production.tfstate"
region = "REPLACE_ME"
