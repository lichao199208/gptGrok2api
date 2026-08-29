#!/bin/sh
cp docker-compose.go.yml.orig docker-compose.go.rollback-copy.yml
cp docker-compose.go.rollback-copy.yml docker-compose.go.yml
