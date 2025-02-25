#!/bin/bash

# This script starts core0

# set the genesis time to current time for pruning to work properly
new_time=$(date -u +"%Y-%m-%dT%H:%M:%S.%N")"Z"
jq --arg new_time "$new_time" '.genesis_time = $new_time' /opt/config/genesis_template.json > /opt/config/genesis.json

if [[ ! -f /opt/data/priv_validator_state.json ]]
then
    mkdir /opt/data
    cat <<EOF > /opt/data/priv_validator_state.json
{
  "height": "0",
  "round": 0,
  "step": 0
}
EOF
fi

{
  # wait for the node to get up and running
  while true
  do
    status_code=$(curl --write-out '%{http_code}' --silent --output /dev/null localhost:26657/status)
    if [[ "${status_code}" -eq 200 ]] ; then
      break
    fi
    echo "Waiting for node to be up..."
    sleep 2s
  done

  VAL_ADDRESS=$(celestia-appd keys show core0 --keyring-backend test --bech=val --home /opt -a)

  # Register the validator EVM address
  celestia-appd tx qgb register \
    "${VAL_ADDRESS}" \
    0x966e6f22781EF6a6A82BBB4DB3df8E225DfD9488 \
    --from core0 \
    --home /opt \
    --fees "30000utia" \
    -b block \
    --chain-id="qgb-e2e" \
    --yes
} &

/bin/celestia-appd start \
  --moniker core0 \
  --rpc.laddr tcp://0.0.0.0:26657 \
  --home /opt
