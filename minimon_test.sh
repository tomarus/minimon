#!/bin/bash
echo Debug stdout
echo Debug stderr >&2
# Randomly exit
exit $[ RANDOM % 4 ]
