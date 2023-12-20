#!/usr/bin/env bash

cd generated && buf mod update && buf generate
