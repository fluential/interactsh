#!/bin/bash

# Collect memory profiles every 1 hour
while true; do 
echo "Collecting profiles at $(date '+%Y-%m-%d %H:%M:%S')"
curl http://localhost:8082/debug/pprof/heap > heap-$(date '+%Y-%m-%d-%H:%M:%S').out; sleep 3600; 
done