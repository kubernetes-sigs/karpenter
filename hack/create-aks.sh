#!/usr/bin/env bash
export AZURE_SUBSCRIPTION_ID=
export AZURE_LOCATION=
export AZURE_RESOURCE_GROUP=
export AZURE_ACR_NAME=
export AZURE_CLUSTER_NAME=

az login
az account set --subscription ${AZURE_SUBSCRIPTION_ID}

az group create --name ${AZURE_RESOURCE_GROUP} --location ${AZURE_LOCATION} -o none

az acr create --name ${AZURE_ACR_NAME} --resource-group ${AZURE_RESOURCE_GROUP} --sku Basic --admin-enabled -o none
az acr login --name ${AZURE_ACR_NAME}

az aks create --name ${AZURE_CLUSTER_NAME} --resource-group ${AZURE_RESOURCE_GROUP} --attach-acr ${AZURE_ACR_NAME} --enable-managed-identity --node-count 3 --generate-ssh-keys -o none --network-dataplane cilium --network-plugin azure --network-plugin-mode overlay
az aks get-credentials --name ${AZURE_CLUSTER_NAME} --resource-group ${AZURE_RESOURCE_GROUP} --overwrite-existing

skaffold config set default-repo ${AZURE_ACR_NAME}.azurecr.io/karpenter

