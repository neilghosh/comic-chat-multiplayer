#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Missing ${ENV_FILE}. Copy infra/env.example to infra/.env and fill values."
  exit 1
fi

# shellcheck disable=SC1090
source "${ENV_FILE}"

required=(
  GCP_PROJECT_ID
  GCP_REGION
  CLOUD_RUN_SERVICE
  ARTIFACT_REPO
  GCS_BUCKET_NAME
  GCP_DEPLOY_SERVICE_ACCOUNT
  GCP_RUNTIME_SERVICE_ACCOUNT
)

for key in "${required[@]}"; do
  if [[ -z "${!key:-}" ]]; then
    echo "Missing required value: ${key}"
    exit 1
  fi
done

echo "Enabling required APIs..."
gcloud services enable \
  run.googleapis.com \
  artifactregistry.googleapis.com \
  iamcredentials.googleapis.com \
  iam.googleapis.com \
  cloudresourcemanager.googleapis.com \
  firebase.googleapis.com \
  --project "${GCP_PROJECT_ID}"

echo "Ensuring service accounts exist..."
for sa in "${GCP_DEPLOY_SERVICE_ACCOUNT}" "${GCP_RUNTIME_SERVICE_ACCOUNT}"; do
  gcloud iam service-accounts describe "${sa}" --project "${GCP_PROJECT_ID}" >/dev/null 2>&1 || \
  gcloud iam service-accounts create "${sa%@*}" \
    --display-name="${sa%@*}" \
    --project "${GCP_PROJECT_ID}"
done

echo "Ensuring Artifact Registry repo exists..."
gcloud artifacts repositories describe "${ARTIFACT_REPO}" \
  --location "${GCP_REGION}" \
  --project "${GCP_PROJECT_ID}" >/dev/null 2>&1 || \
gcloud artifacts repositories create "${ARTIFACT_REPO}" \
  --repository-format=docker \
  --location "${GCP_REGION}" \
  --description="Docker images for comic chat" \
  --project "${GCP_PROJECT_ID}"

echo "Ensuring GCS bucket exists..."
gcloud storage buckets describe "gs://${GCS_BUCKET_NAME}" \
  --project "${GCP_PROJECT_ID}" >/dev/null 2>&1 || \
gcloud storage buckets create "gs://${GCS_BUCKET_NAME}" \
  --location "${GCP_REGION}" \
  --uniform-bucket-level-access \
  --project "${GCP_PROJECT_ID}"

echo "Granting deploy service account roles..."
for role in \
  roles/run.admin \
  roles/artifactregistry.admin \
  roles/storage.admin \
  roles/firebasehosting.admin \
  roles/iam.serviceAccountUser; do
  gcloud projects add-iam-policy-binding "${GCP_PROJECT_ID}" \
    --member "serviceAccount:${GCP_DEPLOY_SERVICE_ACCOUNT}" \
    --role "${role}" >/dev/null
done

echo "Granting bucket access to runtime identity..."
gcloud storage buckets add-iam-policy-binding "gs://${GCS_BUCKET_NAME}" \
  --member "serviceAccount:${GCP_RUNTIME_SERVICE_ACCOUNT}" \
  --role "roles/storage.objectUser" \
  --project "${GCP_PROJECT_ID}" >/dev/null

if [[ -n "${GCP_PROJECT_NUMBER:-}" && -n "${GCP_WORKLOAD_IDENTITY_POOL:-}" && -n "${GCP_WORKLOAD_IDENTITY_PROVIDER_ID:-}" && -n "${GITHUB_REPOSITORY:-}" ]]; then
  echo "Ensuring Workload Identity pool/provider exist..."
  gcloud iam workload-identity-pools describe "${GCP_WORKLOAD_IDENTITY_POOL}" \
    --location=global \
    --project="${GCP_PROJECT_ID}" >/dev/null 2>&1 || \
  gcloud iam workload-identity-pools create "${GCP_WORKLOAD_IDENTITY_POOL}" \
    --location=global \
    --project="${GCP_PROJECT_ID}" \
    --display-name="GitHub OIDC pool"

  gcloud iam workload-identity-pools providers describe "${GCP_WORKLOAD_IDENTITY_PROVIDER_ID}" \
    --workload-identity-pool="${GCP_WORKLOAD_IDENTITY_POOL}" \
    --location=global \
    --project="${GCP_PROJECT_ID}" >/dev/null 2>&1 || \
  gcloud iam workload-identity-pools providers create-oidc "${GCP_WORKLOAD_IDENTITY_PROVIDER_ID}" \
    --workload-identity-pool="${GCP_WORKLOAD_IDENTITY_POOL}" \
    --location=global \
    --project="${GCP_PROJECT_ID}" \
    --display-name="GitHub OIDC provider" \
    --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
    --attribute-condition="attribute.repository == '${GITHUB_REPOSITORY}'"

  member="principalSet://iam.googleapis.com/projects/${GCP_PROJECT_NUMBER}/locations/global/workloadIdentityPools/${GCP_WORKLOAD_IDENTITY_POOL}/attribute.repository/${GITHUB_REPOSITORY}"

  echo "Granting WIF principal access to deploy service account..."
  gcloud iam service-accounts add-iam-policy-binding "${GCP_DEPLOY_SERVICE_ACCOUNT}" \
    --project "${GCP_PROJECT_ID}" \
    --role "roles/iam.workloadIdentityUser" \
    --member "${member}" >/dev/null
  gcloud iam service-accounts add-iam-policy-binding "${GCP_DEPLOY_SERVICE_ACCOUNT}" \
    --project "${GCP_PROJECT_ID}" \
    --role "roles/iam.serviceAccountTokenCreator" \
    --member "${member}" >/dev/null
fi

echo
echo "Bootstrap complete."
echo "Configure GitHub repository variables:"
echo "  GCP_PROJECT_ID=${GCP_PROJECT_ID}"
echo "  GCP_REGION=${GCP_REGION}"
echo "  CLOUD_RUN_SERVICE=${CLOUD_RUN_SERVICE}"
echo "  ARTIFACT_REPO=${ARTIFACT_REPO}"
echo "  GCS_BUCKET_NAME=${GCS_BUCKET_NAME}"
echo "  GCP_DEPLOY_SERVICE_ACCOUNT=${GCP_DEPLOY_SERVICE_ACCOUNT}"
echo "  GCP_RUNTIME_SERVICE_ACCOUNT=${GCP_RUNTIME_SERVICE_ACCOUNT}"
echo "  CLOUD_RUN_PUBLIC_ORIGIN=${CLOUD_RUN_PUBLIC_ORIGIN:-https://<your-cloud-run-url>}"
echo "  FIREBASE_SITE_ID=${FIREBASE_SITE_ID:-<your-firebase-site-id>}"
echo "  CLOUD_RUN_MIN_INSTANCES=1"
echo
echo "Configure GitHub repository secrets:"
echo "  GCP_WORKLOAD_IDENTITY_PROVIDER=projects/${GCP_PROJECT_NUMBER:-<project-number>}/locations/global/workloadIdentityPools/${GCP_WORKLOAD_IDENTITY_POOL:-<pool-id>}/providers/${GCP_WORKLOAD_IDENTITY_PROVIDER_ID:-<provider-id>}"
echo "  GEMINI_API_KEY=<value>"
