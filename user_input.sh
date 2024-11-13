#!/bin/bash

while true; do
    echo -n "Enter something (or 'quit' to exit): "
    read input
    if [ "$input" = "quit" ]; then
        break
    fi
    echo "You entered: $input"
done
