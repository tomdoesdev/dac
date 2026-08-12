// Package fault defines dac's operation error taxonomy: the stable category
// every failure carries, the structured context attached to it, and the
// constructors that produce it.
//
// It deliberately depends on nothing. Every other internal package classifies
// its failures through fault, so keeping it free of project layout, transport,
// and persistence concerns is what lets those packages stay independent of one
// another.
package fault
