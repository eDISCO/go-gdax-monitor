# go-gdax-monitor
Unfinished BTC tracker for GDAX exchange.

## Used libraries:

* https://github.com/preichenberger/go-gdax (For some GDAX interfacing) [pre 0.5 version]
* https://github.com/tidwall/buntdb (For BuntDB)
* https://github.com/gizak/termui (For UI)
## Function

Full internal order book is assembled and kept up to date with GDAX. Synchronisation occurs over websockets. *Open*, *Done*, *Match* and *Change* orders are tracked.

## Example

Example of the incomplete (and poorly implemented) terminal interface:
<p align="center">
    <img src="https://cdn.rawgit.com/eDISCO/go-gdax-monitor/e836ea6b/example.svg">
</p>
