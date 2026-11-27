// variator.js, jpad 2024

"use strict"

class Variator {
	#value   // output value
	
	#min     // output range, minimum
	#max     // output range, maximum
	#i_min   // interval range, minimum
	#i_max   // interval range, maximum

	#target  // target to interpolate to
	#inc     // increment
	#delta   // delta to target
	#done    // interval progress [0.0 .. 1.0]

	constructor(val, min, max, i_min, i_max) {
		this.setRange(min, max)
		this.setInterval(i_min, i_max)
		this.setValue(val)
	}

	value() {
		return this.#value
	}

	nextValue() {
	 	if (this.#done >= 1.0) {
			this.#newTarget()
			this.#newInterval()
		}
		this.#value = this.#target - this.#delta*(1.0-cubic(this.#done))
		this.#done += this.#inc
	
		return this.#value
	}

	// NOTE: it could be better to adapth the range to the new value instead
	// of adapting the value to the range, in case of a conflict?
	setValue(val) {
		this.#value = val
		if (this.#value < this.#min) {
			this.#value = this.#min
		} else if (this.#value > this.#max) {
			this.#value = this.#max
		}
		this.#done = 1.0 // trigger next target
	}

	setInterval(i_min, i_max) {
		// validate interval range
		if (i_min < 0 || i_max < 0) {
			throw new Error("intervals must be positive")
		}
		if (i_min > i_max) {
			throw new Error("i_min must be <= i_max")
		}
		this.#i_min = i_min
		this.#i_max = i_max
	}

	setRange(min, max) {
		// validate range
		if (min > max) {
			throw new Error("min must be <= max")
		}
		this.#min = min
		this.#max = max
		// validate current value
		if (this.#value < this.#min) {
			this.#value = this.#min
			this.#done = 1.0
		} else if (this.#value > this.#max) {
			this.#value = this.#max
			this.#done = 1.0
		}
		// validate current target
		if (this.#target < this.#min || this.#target > this.#max) {
			this.newInterval()
			this.newTarget()
		}
	}

	#newTarget() {
		this.#target = this.#min + (this.#max-this.#min)*Math.random()
		this.#delta = this.#target - this.#value
	}

	#newInterval() {
		if (this.#i_min != this.#i_max) {
			this.#inc = 1.0 / randInt(this.#i_min, this.#i_max)
		} else {
			this.#inc = 1.0 / this.#i_min
		}
		this.#done = this.#inc
	}
}


//// utilities /////////////////////////////////////////////////////////////////

function randInt(min, max) {
    min = Math.ceil(min)
    max = Math.floor(max)
    return Math.floor(Math.random() * (max - min + 1)) + min
}


// 3t² - 2t³
function cubic(t) {
	return t * t * (3.0 - 2.0*t)
}


export {Variator}
